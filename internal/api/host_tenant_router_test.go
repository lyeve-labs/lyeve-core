package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// hostRouting stands in for the multitenant plugin's pre-auth resolver: a
// mapped domain names its tenant, and the instance's own host of an install
// with one tenant falls back to that tenant without naming it.
func hostRouting(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "acme.example.com":
			r = r.WithContext(core.WithTenantID(r.Context(), "acme"))
		case "instance.example.com":
			r = r.WithContext(core.WithFallbackTenantID(r.Context(), "solo"))
		}
		next.ServeHTTP(w, r)
	})
}

// The admin router hands a public plugin route the tenant its host names, and
// no host tenant where the resolver only fell back to the install's one tenant.
// A forged X-Tenant-ID changes neither.
func TestAdminRouter_DeliversTheHostTenantToAPluginRoute(t *testing.T) {
	t.Parallel()

	var tenant, hostTenant string
	var ran bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		tenant = core.TenantIDFromCtx(r.Context())
		hostTenant = core.HostTenantIDFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	routes := []plugin.PluginRoutes{{
		Name: "signin",
		Routes: []plugin.RouteDecl{{
			Method:  http.MethodGet,
			Pattern: "/api/admin/signin/callback",
			Handler: probe,
			Group:   core.GroupPublic,
		}},
	}}

	cfg := testConfig()
	cfg.MultiTenant = true
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)),
		WithPluginRoutes(routes),
		WithPreAuthMiddleware(hostRouting),
	)
	require.NoError(t, err)

	cases := []struct {
		name, host, header   string
		wantTenant, wantHost string
	}{
		{"a mapped domain", "acme.example.com", "", "acme", "acme"},
		{"a mapped domain with a forged header", "acme.example.com", "globex", "acme", "acme"},
		{"the sole tenant fallback", "instance.example.com", "", "solo", ""},
		{"the sole tenant fallback with a forged header", "instance.example.com", "globex", "solo", ""},
		{"an unmapped host with a forged header", "other.example.com", "globex", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, hostTenant, ran = "", "", false
			req := httptest.NewRequest(http.MethodGet, "/api/admin/signin/callback", nil)
			req.Host = tc.host
			if tc.header != "" {
				req.Header.Set("X-Tenant-ID", tc.header)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			require.True(t, ran, "the plugin route never ran, status %d", rec.Code)
			assert.Equal(t, tc.wantTenant, tenant, "request tenant")
			assert.Equal(t, tc.wantHost, hostTenant, "host tenant")
		})
	}
}
