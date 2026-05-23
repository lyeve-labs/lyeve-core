package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// logCapture collects slog JSON output so a test can assert on the access log.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

// requestLines returns the decoded access-log lines captured so far.
func (c *logCapture) requestLines() []map[string]any {
	c.mu.Lock()
	raw := c.buf.String()
	c.mu.Unlock()

	var out []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "request" {
			out = append(out, rec)
		}
	}
	return out
}

// captureAccessLog redirects the default logger into a capture for the
// duration of the test. Not parallel-safe: slog.SetDefault is process-wide.
func captureAccessLog(t *testing.T) *logCapture {
	t.Helper()
	cap := &logCapture{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(cap, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return cap
}

// accessLogProbeRoutes are plugin route declarations covering all four
// mounting groups. They stand in for the plugin surface a real instance grows
// at boot, which is where a route is most likely to be mounted somewhere the
// access log cannot see it.
func accessLogProbeRoutes() []plugin.PluginRoutes {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return []plugin.PluginRoutes{
		{
			Name: "probe-admin",
			Routes: []core.RouteDecl{
				{Method: http.MethodGet, Pattern: "/api/admin/probe/items", Handler: ok, Group: plugin.GroupAdmin},
				{Method: http.MethodGet, Pattern: "/api/admin/probe/items/{id}", Handler: ok, Group: plugin.GroupAdmin},
				{Method: http.MethodPost, Pattern: "/api/admin/probe/items/{id}/ping", Handler: ok, Group: plugin.GroupSuperAdmin},
			},
		},
		{
			Name: "probe",
			Routes: []core.RouteDecl{
				{Method: http.MethodGet, Pattern: "/api/admin/probe/public", Handler: ok, Group: plugin.GroupPublic},
				{Method: http.MethodGet, Pattern: "/api/admin/probe/auth", Handler: ok, Group: plugin.GroupAuth},
				{Method: http.MethodGet, Pattern: "/api/v1/probe/public", Handler: ok, Group: plugin.GroupPublic},
				{Method: http.MethodGet, Pattern: "/api/v1/probe/auth", Handler: ok, Group: plugin.GroupAuth},
			},
		},
	}
}

// concreteRequestPath turns a chi route pattern into a path that can be
// requested: named parameters and wildcards become a literal segment.
func concreteRequestPath(pattern string) string {
	segments := strings.Split(pattern, "/")
	for i, seg := range segments {
		switch {
		case seg == "*":
			segments[i] = "probe"
		case strings.HasPrefix(seg, "{"):
			segments[i] = "probe"
		}
	}
	path := strings.Join(segments, "/")
	if strings.HasSuffix(path, "/") && path != "/" {
		path += "probe"
	}
	return path
}

// walkRoutes returns every method+pattern pair registered on h.
func walkRoutes(t *testing.T, h http.Handler) [][2]string {
	t.Helper()
	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("handler %T does not expose its routing tree", h)
	}
	var out [][2]string
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, [2]string{method, route})
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("router exposed no routes - the walk cannot prove anything")
	}
	return out
}

// assertEveryRouteLogs requests every registered route and fails for each one
// that produced no access-log line.
//
// This is the guard the defect class needs. A route reachable without a log
// entry means it escaped the logging middleware, and the only reliable way to
// find the next one is to enumerate the routing tree rather than to name the
// routes a person happened to think of.
func assertEveryRouteLogs(t *testing.T, router http.Handler, cap *logCapture) {
	t.Helper()
	for _, route := range walkRoutes(t, router) {
		method, pattern := route[0], route[1]
		path := concreteRequestPath(pattern)
		if accessLogExempt(path) {
			continue
		}

		cap.reset()
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		var logged bool
		for _, line := range cap.requestLines() {
			if line["method"] == method && line["path"] == path {
				logged = true
				break
			}
		}
		if !logged {
			t.Errorf("%s %s answered %d with no access-log line; the route is outside the logging chain. "+
				"Mount it under applyAccessLogging, or declare it in accessLogExemptPaths",
				method, path, rec.Code)
		}
	}
}

func TestAccessLog_EveryAdminRouteIsLogged(t *testing.T) {
	cap := captureAccessLog(t)

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)),
		WithPluginRoutes(accessLogProbeRoutes()))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	assertEveryRouteLogs(t, router, cap)
}

func TestAccessLog_EveryAPIRouteIsLogged(t *testing.T) {
	cap := captureAccessLog(t)

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)),
		WithPluginRoutes(accessLogProbeRoutes()))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}
	assertEveryRouteLogs(t, router, cap)
}

// A path no route claims still reaches a handler (the router's own 404), and
// that answer belongs in the log as much as any other. Probing for endpoints
// is what a scan looks like, and it is invisible if unrouted paths are silent.
func TestAccessLog_UnroutedPathIsLogged(t *testing.T) {
	cap := captureAccessLog(t)

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/no-such-endpoint", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	lines := cap.requestLines()
	if len(lines) != 1 {
		t.Fatalf("access-log lines = %d, want 1", len(lines))
	}
	if got := lines[0]["status"]; got != float64(http.StatusNotFound) {
		t.Errorf("logged status = %v, want 404", got)
	}
}

// The guards that answer without calling the next handler sit below the
// logging middleware, so a rate-limited or oversize request is refused and
// still recorded.
func TestAccessLog_RefusedRequestIsLogged(t *testing.T) {
	tests := []struct {
		name       string
		config     func(*config.Config)
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "oversize body",
			config:     func(c *config.Config) { c.MaxBodyBytes = 8 },
			method:     http.MethodPost,
			path:       "/api/admin/auth/login",
			body:       strings.Repeat("x", 512),
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		{
			name: "rate limited",
			config: func(c *config.Config) {
				c.RateLimitRPS = 1
				c.RateLimitBurst = 1
			},
			method:     http.MethodGet,
			path:       "/api/admin/setup",
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "ip not allowlisted",
			config:     func(c *config.Config) { c.IPAllowlist = []string{"10.0.0.0/8"} },
			method:     http.MethodGet,
			path:       "/api/admin/setup",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "fullwidth homoglyph in path",
			config:     func(c *config.Config) {},
			method:     http.MethodGet,
			path:       "/api/admin/\uff33etup",
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cap := captureAccessLog(t)

			cfg := testConfig()
			tc.config(cfg)
			router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)))
			if err != nil {
				t.Fatalf("NewAdminRouter: %v", err)
			}

			// The rate limiter refuses only once the budget is spent, so
			// drive it until the guard answers or the attempt count runs out.
			var rec *httptest.ResponseRecorder
			for attempt := 0; attempt < 8; attempt++ {
				cap.reset()
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				rec = httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code == tc.wantStatus {
					break
				}
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d - the guard under test never refused", rec.Code, tc.wantStatus)
			}

			lines := cap.requestLines()
			if len(lines) == 0 {
				t.Fatalf("%s %s was refused with %d and produced no access-log line",
					tc.method, tc.path, rec.Code)
			}
			if got := lines[len(lines)-1]["status"]; got != float64(tc.wantStatus) {
				t.Errorf("logged status = %v, want %d", got, tc.wantStatus)
			}
		})
	}
}

// A refusal has to be traceable back to the caller who saw it, which means the
// request id on the response and the one in the log must be the same value.
func TestAccessLog_RefusalCarriesTheResponseRequestID(t *testing.T) {
	cap := captureAccessLog(t)

	cfg := testConfig()
	cfg.MaxBodyBytes = 8
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(strings.Repeat("x", 512)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	responseID := rec.Header().Get("X-Request-ID")
	if responseID == "" {
		t.Fatal("refusal carried no X-Request-ID")
	}
	lines := cap.requestLines()
	if len(lines) != 1 {
		t.Fatalf("access-log lines = %d, want 1", len(lines))
	}
	if got := lines[0]["request_id"]; got != responseID {
		t.Errorf("logged request_id = %v, response X-Request-ID = %q - a refusal cannot be traced", got, responseID)
	}
}

// A stale exemption is as bad as an accidental one: it names a path nobody
// serves any more and quietly excuses whatever is mounted there next.
func TestAccessLog_ExemptPathsAreRoutesThatExist(t *testing.T) {
	if len(accessLogExemptPaths) == 0 {
		return
	}

	admin, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	registered := make(map[string]bool)
	for _, h := range []http.Handler{admin, apiRouter} {
		for _, route := range walkRoutes(t, h) {
			registered[concreteRequestPath(route[1])] = true
		}
	}

	for path := range accessLogExemptPaths {
		if !registered[path] {
			t.Errorf("accessLogExemptPaths names %q, which no router serves - remove the stale entry", path)
		}
	}
}
