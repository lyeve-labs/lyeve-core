package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// memorySink keeps every capture entry, for a test that reads what was stored.
type memorySink struct {
	mu      sync.Mutex
	entries []observability.CaptureEntry
}

func (s *memorySink) Capture(e observability.CaptureEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

// bodiesStored reports whether the last entry for path kept either body.
func (s *memorySink) bodiesStored(t *testing.T, path string) bool {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.entries) - 1; i >= 0; i-- {
		if e := s.entries[i]; e.URL == path {
			return len(e.RequestBody) > 0 || len(e.ResponseBody) > 0
		}
	}
	t.Fatalf("nothing was captured for %s; the request itself must be recorded", path)
	return false
}

// refuseMarked stands in for a plugin middleware that refuses a request
// before routing, as a firewall does.
func refuseMarked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Refuse") != "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"refused"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sensitiveProbeRoutes(prefix string) []plugin.PluginRoutes {
	echo := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"secret":"shown-once"}`))
	})
	return []plugin.PluginRoutes{{Name: "probe", Routes: []plugin.RouteDecl{
		{Method: http.MethodPost, Pattern: prefix + "/widgets/{id}/secret", Group: plugin.GroupAuth, Handler: echo, Sensitive: true},
		{Method: http.MethodPost, Pattern: prefix + "/widgets/{id}", Group: plugin.GroupAuth, Handler: echo},
		{Method: http.MethodPost, Pattern: prefix + "/open-widgets/secret", Group: plugin.GroupPublic, Handler: echo, Sensitive: true},
	}}}
}

// A route that declares its bodies sensitive keeps them out of the capture on
// both routers, including when a middleware refuses the request before it is
// routed. A route beside it that declares nothing is captured in full.
func TestSensitiveRoute_BodiesStayOutOfTheCapture(t *testing.T) {
	type build func(t *testing.T, opts ...RouterOption) http.Handler
	admin := func(t *testing.T, opts ...RouterOption) http.Handler {
		h, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), append(opts, WithLifetime(testLifetime(t)))...)
		require.NoError(t, err)
		return h
	}
	api := func(t *testing.T, opts ...RouterOption) http.Handler {
		h, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, append(opts, WithLifetime(testLifetime(t)))...)
		require.NoError(t, err)
		return h
	}
	for _, rt := range []struct {
		name   string
		prefix string
		build  build
	}{
		{"admin", "/api/admin", admin},
		{"api", "/api/v1", api},
	} {
		t.Run(rt.name, func(t *testing.T) {
			sink := &memorySink{}
			h := rt.build(t,
				WithPluginRoutes(sensitiveProbeRoutes(rt.prefix)),
				WithRequestCapture(sink),
				WithMiddleware(refuseMarked),
			)
			send := func(path string, refused bool) {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"code":"123456"}`))
				req.Header.Set("Content-Type", "application/json")
				if refused {
					req.Header.Set("X-Refuse", "1")
				}
				h.ServeHTTP(httptest.NewRecorder(), req)
			}

			cases := []struct {
				name    string
				path    string
				refused bool
				stored  bool
			}{
				{"sensitive", rt.prefix + "/widgets/7/secret", false, false},
				{"sensitive refused before routing", rt.prefix + "/widgets/8/secret", true, false},
				{"sensitive public", rt.prefix + "/open-widgets/secret", false, false},
				{"undeclared sibling", rt.prefix + "/widgets/7", false, true},
				{"undeclared sibling refused before routing", rt.prefix + "/widgets/8", true, true},
			}
			for _, c := range cases {
				send(c.path, c.refused)
				require.Equal(t, c.stored, sink.bodiesStored(t, c.path), c.name)
			}
		})
	}
}

// With no route declaring anything, the capture pays for no pattern lookup.
func TestSensitiveRoutes_NilWhenNothingIsDeclared(t *testing.T) {
	routes := sensitiveProbeRoutes("/api/v1")
	for i := range routes[0].Routes {
		routes[0].Routes[i].Sensitive = false
	}
	require.Nil(t, sensitiveRoutes(nil, routes))
	require.Nil(t, sensitiveRoutes(nil, nil))
}
