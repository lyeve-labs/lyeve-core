// Plugin route mount and rate limiter integration tests.
//
// Verifies that a plugin's route patterns spelled with the /api/admin prefix
// are mounted by mountPluginRoutes and that the public rate limiter wraps them
// under the keys their declared limits carry.

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// signInRoutes are a provider's sign-in redirect and callback, spelled with
// the /api/admin prefix and declaring their own limit.
func signInRoutes() []plugin.PluginRoutes {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	limit := &core.RouteRateLimit{Rate: 20, Burst: 30}
	return []plugin.PluginRoutes{{
		Name: "oauth",
		Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/auth/oauth/{provider}", Group: plugin.GroupPublic, Handler: ok, RateLimit: limit},
			{Method: "GET", Pattern: "/api/admin/auth/oauth/{provider}/callback", Group: plugin.GroupPublic, Handler: ok, RateLimit: limit},
		},
	}}
}

// Plugin routes with the /api/admin prefix mount as non-404 and get wrapped
// by the public rate limiter (RateLimit-Limit header present).
func TestPrefixedPluginRoutes_Mounted(t *testing.T) {
	t.Parallel()

	routes := signInRoutes()
	configs, global := publicRateLimitTable(applyRouterOptions([]RouterOption{WithPluginRoutes(routes)}))
	limiter := apimw.NewPublicEndpointRateLimiter(configs, global, nil)

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", false, 1<<20, limiter, nil, nil)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"redirect_endpoint_mounted", http.MethodGet, "/auth/oauth/github", http.StatusOK},
		{"callback_endpoint_mounted", http.MethodGet, "/auth/oauth/github/callback", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			// Rate limiter should have wrapped this route.
			if got := rec.Header().Get("RateLimit-Limit"); got == "" {
				t.Error("RateLimit-Limit header missing - rate limiter did not wrap this route")
			}
			if got := rec.Header().Get("RateLimit-Remaining"); got == "" {
				t.Error("RateLimit-Remaining header missing - rate limiter did not wrap this route")
			}
		})
	}
}

// A declared limit is keyed by the route's own pattern, with the /api/admin
// prefix the limiter wraps the route under. A key without it would leave the
// limiter passing the route through.
func TestPrefixedPluginRoutes_DeclaredRateLimits_Keys(t *testing.T) {
	t.Parallel()

	configs, _ := publicRateLimitTable(applyRouterOptions([]RouterOption{WithPluginRoutes(signInRoutes())}))

	required := []string{
		"GET:/api/admin/auth/oauth/{provider}",
		"GET:/api/admin/auth/oauth/{provider}/callback",
	}
	for _, key := range required {
		if _, ok := configs[key]; !ok {
			t.Errorf("the public limit table is missing key %q: the limiter passes the route through", key)
		}
	}

	// A limit keyed by the unprefixed pattern would leave the route without its rate limit.
	unprefixed := []string{
		"GET:/auth/oauth/{provider}",
		"GET:/auth/oauth/{provider}/callback",
	}
	for _, key := range unprefixed {
		if _, ok := configs[key]; ok {
			t.Errorf("the public limit table has the unprefixed key %q, which should carry the /api/admin prefix", key)
		}
	}
}
