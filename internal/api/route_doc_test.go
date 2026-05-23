package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// A declared Doc supplies the text of the operation, and the route's group
// and owner still decide its security, roles and license feature.
func TestRouteDoc_DescribesTheOperation(t *testing.T) {
	// No option reports widgets ungated, so the document reads it as a
	// licensed plugin.
	doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "widgets", Routes: []plugin.RouteDecl{{
		Method: http.MethodPost, Pattern: "/api/admin/widgets/indexes/{index}/rebuild", Group: plugin.GroupSuperAdmin, Handler: servesOK(),
		Doc: &core.RouteDoc{
			Tag: "Admin / Widgets", Summary: "Rebuild one index", Description: "Reads every document again.",
			Params: []core.RouteParam{
				{Name: "index", In: "path", Format: "uuid"},
				{Name: "batch", In: "query", Type: "integer", Default: 500},
			},
			RequestExample: map[string]any{"dry_run": true},
			Responses:      map[int]string{http.StatusAccepted: "Rebuild started", http.StatusConflict: "Already running"},
		},
	}}}}, openAPIOptions{})

	op := doc.Paths["/api/admin/widgets/indexes/{index}/rebuild"]["post"]
	require.NotNil(t, op)
	assert.Equal(t, []string{"Admin / Widgets"}, op.Tags)
	assert.Equal(t, "Rebuild one index", op.Summary)
	assert.Equal(t, "Reads every document again.", op.Description)
	assert.Equal(t, []openAPIParameter{
		{Name: "index", In: "path", Required: true, Schema: map[string]any{"type": "string", "format": "uuid"}},
		{Name: "batch", In: "query", Schema: map[string]any{"type": "integer", "default": 500}},
	}, op.Parameters)
	require.NotNil(t, op.RequestBody)
	assert.True(t, op.RequestBody.Required)
	assert.Equal(t, map[string]any{"dry_run": true}, op.RequestBody.Content["application/json"].Schema["example"])
	assert.Equal(t, map[string]any{
		"202": map[string]any{"description": "Rebuild started"},
		"409": map[string]any{"description": "Already running"},
	}, op.Responses)

	assert.Equal(t, []map[string][]any{{"bearerAuth": {}}, {"cookieAuth": {}}}, op.Security)
	assert.Equal(t, []string{"super_admin"}, op.Extensions[xRoles])
	assert.Equal(t, "widgets", op.Extensions[xFeature])
}

// A path segment the Doc leaves out is still documented, ahead of what the
// Doc names, and a path parameter is required whatever the Doc says.
func TestRouteDoc_EveryPathSegmentIsDocumented(t *testing.T) {
	doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "apikey", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/keys/{owner}/{key:[a-z0-9]+}", Group: plugin.GroupAdmin, Handler: servesOK(),
			Doc: &core.RouteDoc{Params: []core.RouteParam{
				{Name: "key", In: "path", Required: false},
				{Name: "reveal", In: "query", Type: "boolean"},
			}}},
	}}}, openAPIOptions{})

	op := doc.Paths["/api/admin/keys/{owner}/{key:[a-z0-9]+}"]["get"]
	require.NotNil(t, op)
	require.Len(t, op.Parameters, 3)
	assert.Equal(t, openAPIParameter{Name: "owner", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, op.Parameters[0])
	assert.Equal(t, openAPIParameter{Name: "key", In: "path", Required: true, Schema: map[string]any{"type": "string"}}, op.Parameters[1])
	assert.Equal(t, "reveal", op.Parameters[2].Name)
	assert.Equal(t, "GET /api/admin/keys/{owner}/{key:[a-z0-9]+}", op.Summary, "a Doc with no summary keeps the built one")
}

// A plugin route's Doc describes its operation, method by method. A method
// it declares with no Doc is described from its method and pattern, and an
// engine route keeps the engine's text whatever a plugin declares, because
// the engine's handler serves it.
func TestRouteDoc_TakesThePlaceOfTheEnginesTextForItsRoute(t *testing.T) {
	doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "webhook", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/webhooks", Group: plugin.GroupAdmin, Handler: servesOK(),
			Doc: &core.RouteDoc{Tag: "Admin / Webhooks", Summary: "List the tenant's webhooks"}},
		{Method: http.MethodPost, Pattern: "/api/admin/webhooks", Group: plugin.GroupAdmin, Handler: servesOK()},
		{Method: http.MethodGet, Pattern: "/api/admin/users", Group: plugin.GroupAdmin, Handler: servesOK(),
			Doc: &core.RouteDoc{Summary: "A plugin's idea of the user list"}},
	}}}, openAPIOptions{})

	assert.Equal(t, "List the tenant's webhooks", doc.Paths["/api/admin/webhooks"]["get"].Summary)
	assert.Equal(t, "POST /api/admin/webhooks", doc.Paths["/api/admin/webhooks"]["post"].Summary, "an undescribed method is described from its method and pattern")
	assert.Equal(t, []string{"Plugin / webhook"}, doc.Paths["/api/admin/webhooks"]["post"].Tags)
	assert.Equal(t, "List all users", doc.Paths["/api/admin/users"]["get"].Summary, "an engine route keeps the engine's text")
}

// The engine writes no text for a route it does not serve. A plugin route
// that is not declared is not in the document, and a route that is declared
// is described by its declaration alone.
func TestRouteDoc_EngineWritesNothingForAPluginRoute(t *testing.T) {
	engineOnly := buildOpenAPIDoc(nil, nil, openAPIOptions{})
	for _, path := range []string{"/api/admin/webhooks", "/api/admin/api-keys", "/api/admin/mfa/status", "/api/admin/tenants", "/api/admin/media", "/api/admin/audit-log", "/api/v1/webhooks/in/{id}"} {
		assert.NotContains(t, engineOnly.Paths, path)
	}

	declared := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "webhook", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/webhooks", Group: plugin.GroupAdmin, Handler: servesOK()},
	}}}, openAPIOptions{})
	require.Contains(t, declared.Paths, "/api/admin/webhooks")
	assert.Len(t, declared.Paths["/api/admin/webhooks"], 1, "only the declared method is documented")
	assert.Equal(t, "GET /api/admin/webhooks", declared.Paths["/api/admin/webhooks"]["get"].Summary)
}
