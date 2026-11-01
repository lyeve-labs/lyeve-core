package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func signInRoutesOf(t *testing.T, routes []plugin.PluginRoutes) (int, []signInRoute) {
	t.Helper()
	return signInRoutesFor(t, routes, unlicensedEntitlements{}, "")
}

// signInRoutesFor asks the route as a request the host resolved to tenant.
func signInRoutesFor(t *testing.T, routes []plugin.PluginRoutes, ent EntitlementProvider, tenant string) (int, []signInRoute) {
	t.Helper()
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)), WithPluginRoutes(routes), WithEntitlements(ent))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/sign-in-routes", nil)
	if tenant != "" {
		req = req.WithContext(core.WithTenantID(req.Context(), tenant))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var body struct {
		Routes []signInRoute `json:"routes"`
	}
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.NotNil(t, body.Routes, "an empty list is an empty array, never null")
	}
	return rec.Code, body.Routes
}

// A sign-in page learns which ways in are mounted from one read that answers
// 200, instead of sending each route a request it refuses.
func TestSignInRoutes_ListsThePublicAuthRoutesThePluginsMounted(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	code, routes := signInRoutesOf(t, []plugin.PluginRoutes{
		{Name: "password-reset", Routes: []plugin.RouteDecl{
			{Method: "POST", Pattern: "/api/admin/auth/password-reset/request", Group: plugin.GroupPublic, Handler: ok},
		}},
		{Name: "magic-link", Routes: []plugin.RouteDecl{
			{Method: "POST", Pattern: "/api/admin/auth/magic-link/request", Group: plugin.GroupPublic, Handler: ok},
			// Signed in already, so not a way in.
			{Method: "GET", Pattern: "/api/admin/auth/magic-link/settings", Group: plugin.GroupAdmin, Handler: ok},
		}},
		{Name: "example", Routes: []plugin.RouteDecl{
			// Public, but not under the sign-in prefix.
			{Method: "POST", Pattern: "/api/admin/example/submit", Group: plugin.GroupPublic, Handler: ok},
		}},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []signInRoute{
		{Method: "POST", Path: "/api/admin/auth/magic-link/request"},
		{Method: "POST", Path: "/api/admin/auth/password-reset/request"},
	}, routes)
}

// A plugin the license withholds from the caller's tenant answers its routes
// with a refusal, so the list leaves them out for that tenant and keeps them
// for every other.
func TestSignInRoutes_LeavesOutAPluginWithheldFromTheTenant(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	routes := []plugin.PluginRoutes{
		{Name: "password-reset", Routes: []plugin.RouteDecl{
			{Method: "POST", Pattern: "/api/admin/auth/password-reset/request", Group: plugin.GroupPublic, Handler: ok},
		}},
		{Name: "magic-link", Routes: []plugin.RouteDecl{
			{Method: "POST", Pattern: "/api/admin/auth/magic-link/request", Group: plugin.GroupPublic, Handler: ok},
		}},
	}
	refusals := tenantRefusals{"acme": {"magic-link"}}

	code, got := signInRoutesFor(t, routes, refusals, "acme")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []signInRoute{{Method: "POST", Path: "/api/admin/auth/password-reset/request"}}, got)

	code, got = signInRoutesFor(t, routes, refusals, "other")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []signInRoute{
		{Method: "POST", Path: "/api/admin/auth/magic-link/request"},
		{Method: "POST", Path: "/api/admin/auth/password-reset/request"},
	}, got)
}

// A route with a path parameter is a later step of a sign-in, reached from a
// link or a provider's redirect, never offered on the page, so the public
// answer does not name it.
func TestSignInRoutes_LeavesOutRoutesWithAPathParameter(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	code, got := signInRoutesOf(t, []plugin.PluginRoutes{
		{Name: "oauth", Routes: []plugin.RouteDecl{
			{Method: "GET", Pattern: "/api/admin/auth/oauth-providers", Group: plugin.GroupPublic, Handler: ok},
			{Method: "GET", Pattern: "/api/admin/auth/oauth/{provider}/callback", Group: plugin.GroupPublic, Handler: ok},
		}},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []signInRoute{{Method: "GET", Path: "/api/admin/auth/oauth-providers"}}, got)
}

func TestSignInRoutes_AnswersAnEmptyListWhenNoPluginMountsOne(t *testing.T) {
	code, routes := signInRoutesOf(t, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, routes)
}
