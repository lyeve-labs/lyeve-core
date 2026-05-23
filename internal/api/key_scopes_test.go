package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func TestKeyScopeCatalog_KernelRoutesAreServed(t *testing.T) {
	t.Parallel()

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, WithLifetime(testLifetime(t)))
	require.NoError(t, err)

	served := map[string]bool{}
	require.NoError(t, chi.Walk(router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+route] = true
		return nil
	}))
	for _, kr := range kernelKeyRoutes {
		assert.True(t, served[kr.method+" "+kr.path], "the catalog lists %s %s, which the API router does not serve", kr.method, kr.path)
	}
}

func TestKeyScopeCatalog_DescribesEachRoute(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	routes := keyScopeCatalog([]plugin.PluginRoutes{{Name: "flow", Routes: []plugin.RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/v1/flows/{slug}", Group: plugin.GroupAuth, Handler: ok},
		{Method: http.MethodPost, Pattern: "/api/v1/flows/hooks/{id}", Group: plugin.GroupPublic, Handler: ok},
		{Method: http.MethodGet, Pattern: "/api/admin/flows", Group: plugin.GroupAdmin, Handler: ok},
	}}, {Name: "graphql", Routes: []plugin.RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/v1/graphql", Group: plugin.GroupAuth, Handler: ok, Scope: "graphql:read"},
		{Method: http.MethodPost, Pattern: "/api/v1/graphql/broken", Group: plugin.GroupAuth, Handler: ok, Scope: "graphql"},
	}}}, nil)

	byRoute := map[string]keyScopeRoute{}
	for _, r := range routes {
		_, twice := byRoute[r.Method+" "+r.Path]
		assert.False(t, twice, "%s %s is listed twice", r.Method, r.Path)
		byRoute[r.Method+" "+r.Path] = r
	}

	assert.Equal(t, keyScopeRoute{
		Method: "POST", Path: "/api/v1/flows/{slug}", Resource: "flows", Name: "{slug}",
		Action: "create", Scope: "flows.{slug}:create", Owner: "flow",
	}, byRoute["POST /api/v1/flows/{slug}"])
	assert.Equal(t, keyScopeRoute{
		Method: "POST", Path: "/api/v1/graphql", Resource: "graphql",
		Action: "read", Scope: "graphql:read", Owner: "graphql", Declared: true,
	}, byRoute["POST /api/v1/graphql"])
	assert.Equal(t, "*:*", byRoute["POST /api/v1/graphql/broken"].Scope, "an unreadable declared scope is reached only by the full wildcard")
	assert.Equal(t, "content.{schema}:delete", byRoute["DELETE /api/v1/content/{schema}/{id}"].Scope)
	assert.Equal(t, "engine", byRoute["GET /api/v1/schemas"].Owner, "the engine's route shadows the plugin's, so it is the one listed")

	_, public := byRoute["POST /api/v1/flows/hooks/{id}"]
	assert.False(t, public, "a public route takes no key")
	_, admin := byRoute["GET /api/admin/flows"]
	assert.False(t, admin, "an admin route refuses a key")
	_, adminAuth := byRoute["GET /api/admin/flows/mine"]
	assert.False(t, adminAuth, "the admin API is not offered to keys")
}

func TestKeyScopesHandler_AnswersTheCatalog(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	keyScopesHandler(nil, nil)(rec, httptest.NewRequest(http.MethodGet, "/api/admin/api-key-scopes", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Actions []string        `json:"actions"`
		Routes  []keyScopeRoute `json:"routes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{"read", "create", "update", "delete"}, body.Actions)
	assert.Len(t, body.Routes, len(kernelKeyRoutes))
}

// A key held to one schema or one endpoint reaches it and is refused its
// neighbors, and a key granted the bare resource reaches every one.
func TestAPIRouter_QualifiedScopesNarrowAKey(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil,
		WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectTestClaims),
		WithPluginRoutes([]plugin.PluginRoutes{{Name: "flow", Routes: []plugin.RouteDecl{
			{Method: http.MethodPost, Pattern: "/api/v1/flows/{slug}", Group: plugin.GroupAuth, Handler: ok},
			{Method: http.MethodPut, Pattern: "/api/v1/flows/{slug}", Group: plugin.GroupAuth, Handler: ok},
		}}}))
	require.NoError(t, err)

	refusedForScope := func(method, path, scopes string) bool {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Scopes", scopes)
		req.Header.Set("X-Test-Roles", "editor")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "insufficient scope")
	}

	cases := []struct {
		name, method, path, scopes string
		refused                    bool
	}{
		{"a schema scope reads its schema", http.MethodGet, "/api/v1/content/posts", "content.posts:read", false},
		{"a schema scope reads one entry of it", http.MethodGet, "/api/v1/content/posts/abc", "content.posts:read", false},
		{"a schema scope is refused another schema", http.MethodGet, "/api/v1/content/pages", "content.posts:read", true},
		{"a read scope is refused a create", http.MethodPost, "/api/v1/content/posts", "content.posts:read", true},
		{"a create scope creates", http.MethodPost, "/api/v1/content/posts", "content.posts:create", false},
		{"a create scope is refused an update", http.MethodPut, "/api/v1/content/posts/abc", "content.posts:create", true},
		{"write still covers an update", http.MethodPut, "/api/v1/content/posts/abc", "content:write", false},
		{"write does not cover a delete", http.MethodDelete, "/api/v1/content/posts/abc", "content:write", true},
		{"the bare resource reads every schema", http.MethodGet, "/api/v1/content/pages", "content:read", false},
		{"an endpoint scope calls its flow", http.MethodPost, "/api/v1/flows/order-sync", "flows.order-sync:create", false},
		{"an endpoint scope is refused another flow", http.MethodPost, "/api/v1/flows/refund", "flows.order-sync:create", true},
		{"a flow create scope is refused an update", http.MethodPut, "/api/v1/flows/order-sync", "flows.order-sync:create", true},
		{"a content scope is refused a flow", http.MethodPost, "/api/v1/flows/order-sync", "content:write", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.refused, refusedForScope(tc.method, tc.path, tc.scopes))
		})
	}
}
