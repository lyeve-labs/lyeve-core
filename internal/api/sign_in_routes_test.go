package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func signInRoutesOf(t *testing.T, routes []plugin.PluginRoutes) (int, []signInRoute) {
	t.Helper()
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/auth/sign-in-routes", nil))
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
		{Name: "forms", Routes: []plugin.RouteDecl{
			// Public, but not under the sign-in prefix.
			{Method: "POST", Pattern: "/api/admin/forms/submit", Group: plugin.GroupPublic, Handler: ok},
		}},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []signInRoute{
		{Method: "POST", Path: "/api/admin/auth/magic-link/request"},
		{Method: "POST", Path: "/api/admin/auth/password-reset/request"},
	}, routes)
}

func TestSignInRoutes_AnswersAnEmptyListWhenNoPluginMountsOne(t *testing.T) {
	code, routes := signInRoutesOf(t, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, routes)
}
