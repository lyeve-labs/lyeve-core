package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestInit_CreatesRegistryWithAllVectors(t *testing.T) {
	ResetForTesting()

	r := Init()
	if r == nil {
		t.Fatal("Init() returned nil registry")
	}

	if RequestsTotal == nil {
		t.Error("RequestsTotal is nil after Init()")
	}
	if RequestDurationSeconds == nil {
		t.Error("RequestDurationSeconds is nil after Init()")
	}
	if RequestsInFlight == nil {
		t.Error("RequestsInFlight is nil after Init()")
	}
	if Registry() != r {
		t.Error("Registry() != Init() return value")
	}
}

func TestHandler_ReturnsValidPrometheusText(t *testing.T) {
	ResetForTesting()
	Init()

	// Prometheus text format hides vectors with zero observations.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	RecordRequest(r, 200, 0.001)

	h := Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "lyeve_requests_total") {
		t.Error("missing lyeve_requests_total metric")
	}
	if !strings.Contains(body, "lyeve_request_duration_seconds") {
		t.Error("missing lyeve_request_duration_seconds metric")
	}
	// lyeve_requests_in_flight won't show unless there's an active in-flight request,
	// so check that the handler at least returns 200 and go_goroutines (which always exists).
	if !strings.Contains(body, "go_goroutines") {
		t.Error("missing go_goroutines metric - registry may be broken")
	}
}

func TestHandler_LazyInitsWhenRegistryIsNil(t *testing.T) {
	ResetForTesting()

	h := Handler()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	RecordRequest(r, 200, 0.001)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "lyeve_requests_total") {
		t.Error("missing lyeve_requests_total after lazy-init")
	}
}

func TestPluginLabel_ExtractsFromContext(t *testing.T) {
	tests := []struct {
		name     string
		ctxValue any
		want     string
	}{
		{
			name:     "no context value",
			ctxValue: nil,
			want:     "core",
		},
		{
			name:     "empty string context value",
			ctxValue: "",
			want:     "core",
		},
		{
			name:     "valid plugin name",
			ctxValue: "webhook",
			want:     "webhook",
		},
		{
			name:     "wrong type in context",
			ctxValue: 42,
			want:     "core",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.ctxValue != nil {
				ctx = context.WithValue(ctx, PluginCtxKey, tt.ctxValue)
			}
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
			got := PluginLabel(r)
			if got != tt.want {
				t.Errorf("PluginLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The label is the slug the request resolved to, read from the slot the
// tenant middleware fills on a context the recording frame never sees.
func TestTenantLabel_ReadsTheSlotTheMiddlewareFilled(t *testing.T) {
	ctx, slot := core.WithTenantSlot(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)

	if got := TenantLabel(r); got != NoTenantLabel {
		t.Fatalf("TenantLabel() before resolution = %q, want %q", got, NoTenantLabel)
	}
	slot.Set("acme")
	if got := TenantLabel(r); got != "acme" {
		t.Errorf("TenantLabel() after resolution = %q, want %q", got, "acme")
	}
}

// A request the chain scoped through TenantHeader carries the slug on its
// own context, and that wins over the slot.
func TestTenantLabel_PrefersTheContextSlug(t *testing.T) {
	ctx, slot := core.WithTenantSlot(context.Background())
	slot.Set("stale")
	var got string
	h := middleware.TenantHeader(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = TenantLabel(r)
	}))
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, &auth.Claims{TenantID: "acme", Roles: []string{"admin"}}))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "acme" {
		t.Errorf("TenantLabel() inside the chain = %q, want %q", got, "acme")
	}
}

func TestPluginLabel_DefaultIsCore(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	if got := PluginLabel(r); got != "core" {
		t.Errorf("PluginLabel() = %q, want %q", got, "core")
	}
}

func TestTenantLabel_NoTenantIsDash(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	if got := TenantLabel(r); got != NoTenantLabel {
		t.Errorf("TenantLabel() = %q, want %q", got, NoTenantLabel)
	}
}

func TestStatusCodeBucket(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{100, "1xx"}, {199, "1xx"},
		{200, "2xx"}, {299, "2xx"},
		{300, "3xx"}, {399, "3xx"},
		{400, "4xx"}, {499, "4xx"},
		{500, "5xx"}, {502, "5xx"}, {599, "5xx"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := statusCodeBucket(tt.code); got != tt.want {
				t.Errorf("statusCodeBucket(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// The recorded series carry the tenant, and a request that resolved none
// carries the placeholder rather than a tenant-shaped word.
func TestRecordRequest_TenantLabelOnEverySeries(t *testing.T) {
	ResetForTesting()
	Init()

	ctx, slot := core.WithTenantSlot(context.Background())
	scoped, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/content/posts", nil)
	slot.Set("acme")
	RecordRequest(scoped, 200, 0.002)
	RecordRequest(httptest.NewRequest(http.MethodGet, "/api/v1/health", nil), 200, 0.001)

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`lyeve_requests_total{code="2xx",method="GET",plugin="core",tenant="acme"} 1`,
		`lyeve_requests_total{code="2xx",method="GET",plugin="core",tenant="-"} 1`,
		`lyeve_request_duration_seconds_count{method="GET",plugin="core",tenant="acme"} 1`,
		`lyeve_request_duration_seconds_count{method="GET",plugin="core",tenant="-"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "tenant_id=") {
		t.Error("the label is tenant, not tenant_id")
	}
}

// Race-condition tests (run with: go test -race)

func TestRaceConcurrentMetricsInit(t *testing.T) {
	// Concurrent Handler() calls trigger lazy-init.
	ResetForTesting()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Handler()
		}()
	}
	wg.Wait()

	if reg == nil {
		t.Fatal("registry still nil after concurrent Handler() calls")
	}
}

func TestRaceConcurrentRecordRequest(t *testing.T) {
	// Concurrent RecordRequest + /metrics scrape.
	ResetForTesting()
	Init()

	const n = 50
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			RecordRequest(r, 200, 0.001)
		}()
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := Handler()
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		}()
	}

	wg.Wait()
}
