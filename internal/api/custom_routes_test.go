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

// pathProvider serves one path and records what it was asked.
type pathProvider struct {
	path  string
	asked []string
}

func (p *pathProvider) CustomRoute(r *http.Request) http.Handler {
	p.asked = append(p.asked, r.URL.Path)
	if r.URL.Path != p.path {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
}

func customRouter(t *testing.T, p core.CustomRouteProvider) http.Handler {
	t.Helper()
	opts := []RouterOption{WithLifetime(testLifetime(t))}
	if p != nil {
		opts = append(opts, WithCustomRoutes(p))
	}
	r, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, opts...)
	require.NoError(t, err)
	return r
}

func serve(r http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestCustomRoutes_AnUnroutedPathReachesTheProvider(t *testing.T) {
	p := &pathProvider{path: "/hooks/stripe"}
	r := customRouter(t, p)

	rec := serve(r, http.MethodPost, "/hooks/stripe")
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("X-Content-Type-Options"), "the outer router's security headers apply to a custom path")

	rec = serve(r, http.MethodGet, "/hooks/other")
	assert.Equal(t, http.StatusNotFound, rec.Code, "a path the provider does not serve is the engine's 404")
	assert.Contains(t, rec.Body.String(), `"not_found"`)
}

// A registered route always wins: the provider is asked only when nothing
// matched, so a custom path can never shadow an engine route.
func TestCustomRoutes_ARegisteredRouteIsNeverOffered(t *testing.T) {
	p := &pathProvider{path: "/.well-known/jwks.json"}
	r := customRouter(t, p)

	rec := serve(r, http.MethodGet, "/.well-known/jwks.json")
	assert.NotEqual(t, http.StatusTeapot, rec.Code)
	assert.NotContains(t, p.asked, "/.well-known/jwks.json")
}

func TestCustomRoutes_NoProviderIsTheEngine404(t *testing.T) {
	rec := serve(customRouter(t, nil), http.MethodGet, "/hooks/stripe")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), `"not_found"`)
}

// A plugin endpoint joins the document beside the declared routes: ANY is
// every method, a declared operation is never replaced, and the group and
// feature read the way a declared route's do.
func TestDocumentEndpoints_AddsBesideTheDeclaredRoutes(t *testing.T) {
	declared := &openAPIOperation{Summary: "declared"}
	paths := map[string]openAPIPath{"/api/v1/flows/{slug}": {"get": declared}}
	documentEndpoints(paths, []core.DocumentedEndpoint{
		{Method: "ANY", Path: "/hooks/stripe", Summary: "Stripe", Tag: "Flow endpoints", Group: core.GroupPublic, Feature: "reports-export", Example: map[string]any{"id": "evt_1"}},
		{Method: "GET", Path: "/api/v1/flows/{slug}", Summary: "orders", Tag: "Flow endpoints", Group: core.GroupAuth},
		{Method: "POST", Path: "/api/v1/flows/{slug}", Summary: "orders", Tag: "Flow endpoints", Group: core.GroupAuth},
		{Method: "", Path: "/skipped"},
	})

	require.Len(t, paths["/hooks/stripe"], 5)
	post := paths["/hooks/stripe"]["post"]
	assert.Empty(t, post.Security, "a public endpoint asks for no credentials")
	assert.Equal(t, "reports-export", post.Extensions[xFeature])
	require.NotNil(t, post.RequestBody)
	assert.Nil(t, paths["/hooks/stripe"]["get"].RequestBody, "a GET carries no body")

	assert.Same(t, declared, paths["/api/v1/flows/{slug}"]["get"], "a declared operation keeps its text")
	require.Contains(t, paths["/api/v1/flows/{slug}"], "post")
	assert.NotEmpty(t, paths["/api/v1/flows/{slug}"]["post"].Security)
	assert.Equal(t, "slug", paths["/api/v1/flows/{slug}"]["post"].Parameters[0].Name)
	assert.NotContains(t, paths, "/skipped")
}

// chainMarker is a middleware of the /api/v1 chain that marks every
// response it sees, so a test can tell whether a request passed that chain.
func chainMarker(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Chain", "v1")
		next.ServeHTTP(w, r)
	})
}

func v1Router(t *testing.T, p core.CustomRouteProvider, routes []plugin.PluginRoutes) http.Handler {
	t.Helper()
	opts := []RouterOption{WithLifetime(testLifetime(t)), WithMiddleware(chainMarker), WithPluginRoutes(routes)}
	if p != nil {
		opts = append(opts, WithCustomRoutes(p))
	}
	r, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, opts...)
	require.NoError(t, err)
	return r
}

func flowLikeRoutes(extra ...core.RouteDecl) []plugin.PluginRoutes {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	decls := []core.RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/v1/flows/{slug}", Handler: ok, Group: plugin.GroupAuth},
		{Method: http.MethodGet, Pattern: "/api/v1/probe/public", Handler: ok, Group: plugin.GroupPublic},
	}
	return []plugin.PluginRoutes{{Name: "probe", Routes: append(decls, extra...)}}
}

// A path under /api/v1 that no route matches reaches the provider before
// the prefix's chain, so it is served without the credentials and tenant
// steps a registered route under /api/v1 passes.
func TestCustomRoutes_AnUnroutedPathUnderV1SkipsThePrefixChain(t *testing.T) {
	p := &pathProvider{path: "/api/v1/orders/sync"}
	r := v1Router(t, p, flowLikeRoutes())

	rec := serve(r, http.MethodPost, "/api/v1/orders/sync")
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Empty(t, rec.Header().Get("X-Test-Chain"), "the /api/v1 chain does not run for a custom path")
	assert.NotEmpty(t, rec.Header().Get("X-Content-Type-Options"), "the outer guards still apply")

	rec = serve(r, http.MethodGet, "/api/v1/probe/public")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "v1", rec.Header().Get("X-Test-Chain"), "a registered route passes the chain")

	rec = serve(r, http.MethodGet, "/api/v1/orders/other")
	assert.Equal(t, http.StatusNotFound, rec.Code, "a path the provider does not serve is the engine's 404")
}

// A registered route under /api/v1 wins for every method: the provider is
// not asked about a path a route matches, and a method the route does not
// take is the engine's 405.
func TestCustomRoutes_ARegisteredRouteUnderV1WinsForEveryMethod(t *testing.T) {
	p := &pathProvider{path: "/api/v1/flows/orders"}
	r := v1Router(t, p, flowLikeRoutes())

	rec := serve(r, http.MethodPost, "/api/v1/flows/orders")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "the flow route answers, behind its auth group")
	rec = serve(r, http.MethodGet, "/api/v1/flows/orders")
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.NotContains(t, p.asked, "/api/v1/flows/orders")
}

func TestRouteIndex_OwnsWhatTheRouterServesAndItsNamespaces(t *testing.T) {
	h := v1Router(t, nil, flowLikeRoutes())
	x := NewRouteIndex(func() http.Handler { return h })

	owned := []string{
		"/api", "/api/v1", "/api/v1/",
		"/api/v1/flows/orders",
		"/api/v1/flows/orders/deeper",
		"/api/v1/content/posts/1/relations/x/y",
		"/api/v1/auth/token", "/api/v1/auth/other",
		"/api/v1/schemas",
		"/api/v1/probe/anything",
		"/.well-known/jwks.json",
		"relative", "",
	}
	for _, p := range owned {
		assert.True(t, x.Owns(p), "%q is the router's", p)
	}
	free := []string{"/api/v1/orders/sync", "/api/v1/xhhsjsyh", "/api/hooks/stripe", "/hooks/stripe"}
	for _, p := range free {
		assert.False(t, x.Owns(p), "%q is no route's", p)
	}
}

// A pattern that opens with a parameter at the namespace position owns
// every path under its parent.
func TestRouteIndex_AParameterNamespaceOwnsItsParent(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := v1Router(t, nil, flowLikeRoutes(core.RouteDecl{Method: http.MethodGet, Pattern: "/api/v1/{kind}/items", Handler: ok, Group: plugin.GroupAuth}))
	x := NewRouteIndex(func() http.Handler { return h })

	assert.True(t, x.Owns("/api/v1/orders/sync"))
	assert.False(t, x.Owns("/api/hooks/stripe"), "the parameter owns /api/v1 only")
}

// A hot reload swaps the router. The index answers from the new one.
func TestRouteIndex_FollowsASwappedRouter(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	sw := NewSwappableHandler(v1Router(t, nil, flowLikeRoutes()))
	x := NewRouteIndex(sw.Current)
	require.False(t, x.Owns("/api/v1/orders/sync"))

	sw.Swap(v1Router(t, nil, flowLikeRoutes(core.RouteDecl{Method: http.MethodGet, Pattern: "/api/v1/orders", Handler: ok, Group: plugin.GroupAuth})))
	assert.True(t, x.Owns("/api/v1/orders/sync"))
}

// A handler the index cannot read owns every path.
func TestRouteIndex_AnUnreadableHandlerOwnsEverything(t *testing.T) {
	x := NewRouteIndex(func() http.Handler { return http.NotFoundHandler() })
	assert.True(t, x.Owns("/api/v1/orders/sync"))
}
