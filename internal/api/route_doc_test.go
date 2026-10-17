package api

import (
	"encoding/json"
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

// A Doc's response media types become the content of each success response
// that carries a body, each with the schema it names, and of no other
// response: an error answers the engine's JSON envelope and a 204 nothing.
func TestRouteDoc_MediaTypesDescribeTheSuccessBody(t *testing.T) {
	row := map[string]any{"type": "object", "required": []any{"id"}}
	doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "reports", Routes: []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/admin/reports/export", Group: plugin.GroupAdmin, Handler: servesOK(),
			Doc: &core.RouteDoc{
				Responses:          map[int]string{http.StatusOK: "The rows", http.StatusBadRequest: "An unknown format"},
				ResponseMediaTypes: []core.RouteMediaType{{MediaType: "text/csv"}, {MediaType: "application/x-ndjson", Schema: row}},
			}},
		{Method: http.MethodPost, Pattern: "/api/admin/reports/runs", Group: plugin.GroupAdmin, Handler: servesOK(),
			Doc: &core.RouteDoc{
				Responses:          map[int]string{http.StatusCreated: "The run, as it goes", http.StatusNoContent: "Nothing to run", http.StatusNotFound: "No such report"},
				ResponseMediaTypes: []core.RouteMediaType{{MediaType: "text/event-stream"}},
			}},
		{Method: http.MethodGet, Pattern: "/api/v1/reports/{id}/chart", Group: plugin.GroupAuth, Handler: servesOK(),
			Doc: &core.RouteDoc{ResponseMediaTypes: []core.RouteMediaType{{MediaType: "image/*"}, {MediaType: "csv"}}}},
	}}}, openAPIOptions{})

	export := doc.Paths["/api/admin/reports/export"]["get"]
	require.NotNil(t, export)
	assert.Equal(t, openAPIResponse{Description: "The rows", Content: map[string]openAPIMediaType{
		"text/csv": {}, "application/x-ndjson": {Schema: row},
	}}, export.Responses["200"])
	assert.Equal(t, map[string]any{"description": "An unknown format"}, export.Responses["400"])

	runs := doc.Paths["/api/admin/reports/runs"]["post"]
	require.NotNil(t, runs)
	assert.Equal(t, openAPIResponse{Description: "The run, as it goes", Content: map[string]openAPIMediaType{"text/event-stream": {}}}, runs.Responses["201"])
	assert.Equal(t, map[string]any{"description": "Nothing to run"}, runs.Responses["204"])
	assert.Equal(t, map[string]any{"description": "No such report"}, runs.Responses["404"])

	// The 200 the document writes for a Doc that names no response takes the
	// types, and one that is not type/subtype is left out.
	chart := doc.Paths["/api/v1/reports/{id}/chart"]["get"]
	require.NotNil(t, chart)
	assert.Equal(t, openAPIResponse{Description: "OK", Content: map[string]openAPIMediaType{"image/*": {}}}, chart.Responses["200"])
}

// A Doc that names no response media type, or only ones the document leaves
// out, is documented exactly as before the field existed: a description per
// response and no content.
func TestRouteDoc_WithoutMediaTypesKeepsItsResponses(t *testing.T) {
	operation := func(types []core.RouteMediaType) string {
		t.Helper()
		doc := buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "reports", Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/admin/reports", Group: plugin.GroupAdmin, Handler: servesOK(),
				Doc: &core.RouteDoc{Summary: "List reports", Responses: map[int]string{http.StatusOK: "The reports", http.StatusForbidden: "Not an admin"}, ResponseMediaTypes: types}},
		}}}, openAPIOptions{})
		raw, err := json.Marshal(doc.Paths["/api/admin/reports"]["get"])
		require.NoError(t, err)
		return string(raw)
	}

	unset := operation(nil)
	var op struct {
		Responses map[string]map[string]any `json:"responses"`
	}
	require.NoError(t, json.Unmarshal([]byte(unset), &op))
	assert.Equal(t, map[string]map[string]any{
		"200": {"description": "The reports"},
		"403": {"description": "Not an admin"},
	}, op.Responses)

	assert.JSONEq(t, unset, operation([]core.RouteMediaType{}))
	assert.JSONEq(t, unset, operation([]core.RouteMediaType{{MediaType: ""}, {MediaType: "csv"}, {MediaType: "*/csv"}}))
}
