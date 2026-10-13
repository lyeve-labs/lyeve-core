package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// operationRoutes declares, on each prefix a router serves, a route that
// answers with the id of the operation it started and with a header it does
// not declare.
func operationRoutes() []plugin.PluginRoutes {
	answer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Operation-Id", "4b1c2d3e")
		w.Header().Set("X-Undeclared", "kept")
		w.WriteHeader(http.StatusOK)
	})
	return []plugin.PluginRoutes{{
		Name: "operations",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/admin/operations/{id}", Group: plugin.GroupPublic,
				Handler: answer, ExposeHeaders: []string{"X-Operation-Id"}},
			{Method: http.MethodGet, Pattern: "/api/v1/operations/{id}", Group: plugin.GroupPublic,
				Handler: answer, ExposeHeaders: []string{"X-Operation-Id"}},
		},
	}}
}

// A script on another origin reads a header only when the response names it
// in Access-Control-Expose-Headers. A route that answers with an operation id
// declares it, and each router then names it beside the configured headers,
// for an allowed origin only.
func TestRouters_ExposeTheHeadersARouteDeclares(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.CORSOrigins = []string{"https://console.example.com"}
	cfg.CORSExposeHeaders = "X-Request-Id, Retry-After"
	routes := operationRoutes()

	admin, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	require.NoError(t, err)
	stateless := NewStatelessRouter(cfg, NewStatelessMode(nil), WithLifetime(testLifetime(t)), WithPluginRoutes(routes))

	cases := []struct {
		name, path string
		router     http.Handler
	}{
		{"admin", "/api/admin/operations/1", admin},
		{"api", "/api/v1/operations/1", apiRouter},
		{"stateless", "/api/v1/operations/1", stateless},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", "https://console.example.com")
			rec := httptest.NewRecorder()
			tc.router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "https://console.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "4b1c2d3e", rec.Header().Get("X-Operation-Id"))
			assert.Equal(t, "X-Request-Id, Retry-After, X-Operation-Id", rec.Header().Get("Access-Control-Expose-Headers"),
				"the declared header joins the configured ones, and an undeclared one stays hidden")

			req = httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", "https://elsewhere.example.net")
			rec = httptest.NewRecorder()
			tc.router.ServeHTTP(rec, req)
			assert.Empty(t, rec.Header().Get("Access-Control-Expose-Headers"), "an origin nobody allowed is told nothing")
		})
	}
}

// The configured list stays as the operator wrote it. A declared name joins it
// once, whatever its case, and a name a browser could not parse is left out,
// because a list it cannot parse exposes nothing at all. The wildcard is left
// out too: without credentials it would expose every header.
func TestExposedHeaders_AddsEachDeclaredNameOnce(t *testing.T) {
	t.Parallel()
	declare := func(names ...string) []plugin.PluginRoutes {
		return []plugin.PluginRoutes{{Name: "p", Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/v1/a", ExposeHeaders: names},
			{Method: http.MethodGet, Pattern: "/api/v1/b", ExposeHeaders: names},
		}}}
	}
	cases := []struct {
		name, configured string
		sets             []plugin.PluginRoutes
		want             string
	}{
		{"nothing declared", "X-Request-Id ,Retry-After", nil, "X-Request-Id ,Retry-After"},
		{"a new name", "X-Request-Id", declare("X-Operation-Id"), "X-Request-Id, X-Operation-Id"},
		{"a name already configured", "X-Request-Id, Retry-After", declare("retry-after"), "X-Request-Id, Retry-After"},
		{"nothing configured", "", declare("X-Operation-Id"), "X-Operation-Id"},
		{"names a browser cannot parse", "X-Request-Id", declare("*", "Bad Header", "", "X-Op;Id"), "X-Request-Id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, exposedHeaders(tc.configured, tc.sets))
		})
	}
}
