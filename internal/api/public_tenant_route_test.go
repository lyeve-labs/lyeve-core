// Package api: the public-route tenant guard and the self-scoping exemption.
//
// A public request names its tenant by the Host header. On an install holding
// more than one tenant, a request whose Host resolved none is answered 404,
// because nobody rostered that hostname. A route that carries its own tenant
// in something signed says so on its declaration and is exempt, because the
// handler that would resolve it has not run at the point the guard decides.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// roster returns a TenantRosterFunc reporting n tenants.
func roster(n int) core.TenantRosterFunc {
	return func(context.Context) (int, error) { return n, nil }
}

// publicRouter mounts one plain and one self-scoping public route behind the
// guard, with the roster the install is meant to have.
func publicRouter(t *testing.T, fn core.TenantRosterFunc) http.Handler {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	routes := []plugin.PluginRoutes{{
		Name: "fixture",
		Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/v1/localization/resolve", Group: plugin.GroupPublic, Handler: ok},
			{Method: "POST", Pattern: "/api/v1/flows/hooks/{flow_id}", Group: plugin.GroupPublic, Handler: ok, SelfScoping: true},
		},
	}}
	r := chi.NewRouter()
	guard := apimw.RequirePublicTenant(true, fn)
	mountPluginRoutes(r, routes, "/api/v1", false, 1<<20, nil, guard, nil)
	mountPluginRoutes(r, routes, "/api/admin", false, 1<<20, nil, guard, nil)
	return r
}

func TestPublicTenantGuard_RefusesOnlyWhenTheRosterIsAmbiguous(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tenants int
		want    int
		why     string
	}{
		{
			name: "no tenants yet", tenants: 0, want: http.StatusOK,
			why: "first-run setup answers before any tenant exists, so an unprovisioned install must still be served",
		},
		{
			name: "one tenant the resolver did not name", tenants: 1, want: http.StatusServiceUnavailable,
			why: "the resolver upstream names a sole tenant; an empty one here means it failed, and several public stores read an empty tenant as the implicit one",
		},
		{
			name: "two tenants", tenants: 2, want: http.StatusNotFound,
			why: "the host was never rostered and there is more than one tenant it could have meant",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			publicRouter(t, roster(tc.tenants)).ServeHTTP(
				rec, httptest.NewRequest(http.MethodGet, "/localization/resolve", nil))
			require.Equal(t, tc.want, rec.Code, tc.why)
		})
	}
}

// The exemption is the whole reason the rule is a route declaration rather
// than a blanket check: a flow's inbound webhook carries its tenant in the
// flow id the handler resolves, and refusing it here would reject a delivery
// that reached the instance under any name but the rostered one.
func TestPublicTenantGuard_SelfScopingRouteIsExempt(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	publicRouter(t, roster(5)).ServeHTTP(
		rec, httptest.NewRequest(http.MethodPost, "/flows/hooks/0f7b2a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b", nil))
	require.Equal(t, http.StatusOK, rec.Code,
		"a self-scoping route must reach its handler on the same install that refuses a plain public route")

	// Negative control: the same roster, the same absent tenant, a route that
	// did not declare itself. Without this the case above would pass just as
	// well if the guard were never mounted at all.
	rec = httptest.NewRecorder()
	publicRouter(t, roster(5)).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/localization/resolve", nil))
	require.Equal(t, http.StatusNotFound, rec.Code,
		"the guard has to be live for the exemption above to mean anything")
}

// A roster that cannot be read says nothing about which tenant a host means.
// A 404 would read to a crawler as the content having been removed, and
// serving it unresolved reaches stores that read an empty tenant as the
// implicit one or drop the predicate. A retryable 503 is what is true.
func TestPublicTenantGuard_RefusesRetryablyWhenTheRosterCannotBeRead(t *testing.T) {
	t.Parallel()

	failing := func(context.Context) (int, error) { return 0, errors.New("roster unavailable") }
	rec := httptest.NewRecorder()
	publicRouter(t, failing).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/localization/resolve", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
}

// Single-tenant installs and installs with no tenant roster never mount the
// guard, so the question does not arise there.
func TestPublicTenantGuard_NotMountedWithoutTenancy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		multiTenant bool
		fn          core.TenantRosterFunc
	}{
		{"single tenant engine", false, roster(9)},
		{"no roster supplier", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			routes := []plugin.PluginRoutes{{Name: "fixture", Routes: []plugin.RouteDecl{
				{Method: "GET", Pattern: "/api/v1/localization/resolve", Group: plugin.GroupPublic, Handler: ok},
			}}}
			r := chi.NewRouter()
			mountPluginRoutes(r, routes, "/api/v1", false, 1<<20, nil,
				apimw.RequirePublicTenant(tc.multiTenant, tc.fn), nil)

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/localization/resolve", nil))
			require.Equal(t, http.StatusOK, rec.Code)
		})
	}
}
