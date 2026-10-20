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
// not declare, and a route that starts an operation under an id the caller
// sends.
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
			{Method: http.MethodPost, Pattern: "/api/admin/operations", Group: plugin.GroupPublic,
				Handler: answer, AllowHeaders: []string{"X-Operation-Id"}},
			{Method: http.MethodPost, Pattern: "/api/v1/operations", Group: plugin.GroupPublic,
				Handler: answer, AllowHeaders: []string{"X-Operation-Id"}},
		},
	}}
}

// operationRouters builds the three routers over operationRoutes, for the
// origin https://console.example.com.
func operationRouters(t *testing.T, exposed, allowed string) map[string]http.Handler {
	t.Helper()
	cfg := testConfig()
	cfg.CORSOrigins = []string{"https://console.example.com"}
	cfg.CORSExposeHeaders = exposed
	cfg.CORSAllowHeaders = allowed
	routes := operationRoutes()

	admin, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	require.NoError(t, err)
	stateless := NewStatelessRouter(cfg, NewStatelessMode(nil), WithLifetime(testLifetime(t)), WithPluginRoutes(routes))
	return map[string]http.Handler{"admin": admin, "api": apiRouter, "stateless": stateless}
}

// A script on another origin reads a header only when the response names it
// in Access-Control-Expose-Headers. A route that answers with an operation id
// declares it, and each router then names it beside the configured headers,
// for an allowed origin only.
func TestRouters_ExposeTheHeadersARouteDeclares(t *testing.T) {
	t.Parallel()
	routers := operationRouters(t, "X-Request-Id, Retry-After", "Content-Type")

	cases := []struct{ router, path string }{
		{"admin", "/api/admin/operations/1"},
		{"api", "/api/v1/operations/1"},
		{"stateless", "/api/v1/operations/1"},
	}
	for _, tc := range cases {
		t.Run(tc.router, func(t *testing.T) {
			router := routers[tc.router]
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", "https://console.example.com")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "https://console.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "4b1c2d3e", rec.Header().Get("X-Operation-Id"))
			assert.Equal(t, "X-Request-Id, Retry-After, X-Operation-Id", rec.Header().Get("Access-Control-Expose-Headers"),
				"the declared header joins the configured ones, and an undeclared one stays hidden")

			req = httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Origin", "https://elsewhere.example.net")
			rec = httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			assert.Empty(t, rec.Header().Get("Access-Control-Expose-Headers"), "an origin nobody allowed is told nothing")
		})
	}
}

// A script on another origin sends a header outside the safelisted set only
// once the preflight allows it. A route that starts an operation under an id
// the caller sends declares the header, and each router's preflight then
// allows it beside the configured headers, for an allowed origin only.
func TestRouters_AllowTheRequestHeadersARouteDeclares(t *testing.T) {
	t.Parallel()
	routers := operationRouters(t, "X-Request-Id", "Content-Type, Authorization")

	cases := []struct{ router, path string }{
		{"admin", "/api/admin/operations"},
		{"api", "/api/v1/operations"},
		{"stateless", "/api/v1/operations"},
	}
	for _, tc := range cases {
		t.Run(tc.router, func(t *testing.T) {
			router := routers[tc.router]
			preflight := func(origin string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
				req.Header.Set("Origin", origin)
				req.Header.Set("Access-Control-Request-Method", http.MethodPost)
				req.Header.Set("Access-Control-Request-Headers", "content-type, x-operation-id")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}

			rec := preflight("https://console.example.com")
			require.Equal(t, http.StatusNoContent, rec.Code)
			require.Equal(t, "https://console.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "Content-Type, Authorization, X-Operation-Id", rec.Header().Get("Access-Control-Allow-Headers"))

			rec = preflight("https://elsewhere.example.net")
			assert.Empty(t, rec.Header().Get("Access-Control-Allow-Headers"), "an origin nobody allowed is told nothing")
		})
	}
}

// The configured list stays as the operator wrote it. A declared name joins it
// once, whatever its case, and a name a browser could not parse is left out,
// because a list it cannot parse is honored not at all. The wildcard is left
// out too: without credentials it would open every header. Both lists follow
// the same rule, each from its own field.
func TestDeclaredCORSHeaders_AddEachNameOnce(t *testing.T) {
	t.Parallel()
	declare := func(names ...string) []plugin.PluginRoutes {
		return []plugin.PluginRoutes{{Name: "p", Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/v1/a", ExposeHeaders: names, AllowHeaders: names},
			{Method: http.MethodGet, Pattern: "/api/v1/b", ExposeHeaders: names, AllowHeaders: names},
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
			assert.Equal(t, tc.want, exposedHeaders(tc.configured, tc.sets), "expose")
			assert.Equal(t, tc.want, allowedHeaders(tc.configured, tc.sets), "allow")
		})
	}

	exposeOnly := []plugin.PluginRoutes{{Name: "p", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/v1/a", ExposeHeaders: []string{"X-Operation-Id"}},
	}}}
	assert.Equal(t, "Content-Type", allowedHeaders("Content-Type", exposeOnly),
		"a header a route answers with is not one a caller may send")
}
