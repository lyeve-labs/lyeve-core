package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// domainRouting stands in for the middleware a plugin contributes through
// core.PreAuthMiddlewareProvider: it maps the Host header onto a tenant slug
// and puts the answer on the context.
func domainRouting(host, tenant string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host == host {
				r = r.WithContext(core.WithTenantID(r.Context(), tenant))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tenantProbe records the tenant a handler mounted below tenancy observes.
func tenantProbe(seen *string, ran *bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*ran = true
			*seen = core.TenantIDFromCtx(r.Context())
			next.ServeHTTP(w, r)
		})
	}
}

// An anonymous request on a multi-tenant install resolves no tenant: it holds
// no claim, and honoring X-Tenant-ID there would turn every public route into
// a tenant enumeration oracle. Domain routing is the supported answer, and it
// only works if the middleware that resolves the domain runs above
// TenantHeader, which reads the context only after its header and claim
// branches come up empty.
func TestAdminRouter_PreAuthMiddlewareResolvesTenantForAnonymousRequest(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.MultiTenant = true

	var seen string
	var ran bool

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)),
		WithPreAuthMiddleware(domainRouting("acme.example.com", "acme")),
		WithMiddleware(tenantProbe(&seen, &ran)),
	)
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil)
	req.Host = "acme.example.com"
	router.ServeHTTP(httptest.NewRecorder(), req)

	if !ran {
		t.Fatal("no middleware below tenancy ran on /api/admin/setup")
	}
	if seen != "acme" {
		t.Errorf("handler saw tenant %q, want %q: pre-auth middleware must run above TenantHeader", seen, "acme")
	}
}

// The same guarantee on the API router, where the public plugin routes the
// anonymous surface is built from are mounted.
func TestAPIRouter_PreAuthMiddlewareResolvesTenantForAnonymousRequest(t *testing.T) {
	t.Parallel()

	cfg := testConfig()

	var seen string
	var ran bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		seen = core.TenantIDFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	routes := []plugin.PluginRoutes{{
		Name: "localization",
		Routes: []plugin.RouteDecl{{
			Method:  http.MethodGet,
			Pattern: "/api/v1/localization/resolve",
			Handler: probe,
			Group:   core.GroupPublic,
		}},
	}}

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)),
		WithPluginRoutes(routes),
		WithPreAuthMiddleware(domainRouting("acme.example.com", "acme")),
	)
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/localization/resolve", nil)
	req.Host = "acme.example.com"
	router.ServeHTTP(httptest.NewRecorder(), req)

	if !ran {
		t.Fatal("public plugin route never reached its handler")
	}
	if seen != "acme" {
		t.Errorf("public route saw tenant %q, want %q: pre-auth middleware must run above TenantHeader", seen, "acme")
	}
}

// A request the plugin does not recognize falls through to the behavior a
// router with no provider has, so adding one cannot change what an install
// already answered on every other host.
func TestAPIRouter_UnmatchedHostKeepsSingleTenantFallback(t *testing.T) {
	t.Parallel()

	cfg := testConfig()

	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = core.TenantIDFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	routes := []plugin.PluginRoutes{{
		Name: "localization",
		Routes: []plugin.RouteDecl{{
			Method:  http.MethodGet,
			Pattern: "/api/v1/localization/resolve",
			Handler: probe,
			Group:   core.GroupPublic,
		}},
	}}

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)),
		WithPluginRoutes(routes),
		WithPreAuthMiddleware(domainRouting("acme.example.com", "acme")),
	)
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/localization/resolve", nil)
	req.Host = "other.example.com"
	router.ServeHTTP(httptest.NewRecorder(), req)

	if seen != apimw.ImplicitTenant {
		t.Errorf("unmatched host saw tenant %q, want the single-tenant fallback %q", seen, apimw.ImplicitTenant)
	}
}

// An install with no provider resolves no tenant ahead of auth. The router is
// built with no pre-auth option at all, which is the shape the runtime
// produces when nothing implements the capability.
func TestAPIRouter_NoPreAuthProviderLeavesTenantUnresolved(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.MultiTenant = true

	var seen string
	var ran bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		seen = core.TenantIDFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	routes := []plugin.PluginRoutes{{
		Name: "localization",
		Routes: []plugin.RouteDecl{{
			Method:  http.MethodGet,
			Pattern: "/api/v1/localization/resolve",
			Handler: probe,
			Group:   core.GroupPublic,
		}},
	}}

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)),
		WithPluginRoutes(routes),
	)
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/localization/resolve", nil)
	req.Host = "acme.example.com"
	router.ServeHTTP(httptest.NewRecorder(), req)

	if !ran {
		t.Fatal("public plugin route never reached its handler")
	}
	if seen != "" {
		t.Errorf("multi-tenant anonymous request resolved tenant %q with no provider wired; want unresolved", seen)
	}
}
