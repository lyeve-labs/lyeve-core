package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// engineDocWithEverything builds the engine's document with every optional
// family mounted and no plugin.
func engineDocWithEverything() openAPIDoc {
	return buildOpenAPIDoc(nil, nil, openAPIOptions{TenantFeatures: true, LicensePresentation: true, AdminTokens: true})
}

// Every operation the engine documents itself names an operation id and an
// effect, so a client or a model can call it by name and knows whether it
// changes data before it calls it.
func TestOpenAPI_EveryEngineOperationIsDescribed(t *testing.T) {
	var missing []string
	for path, methods := range engineDocWithEverything().Paths {
		for method, op := range methods {
			if op.OperationID == "" || op.Extensions[xEffect] == nil {
				missing = append(missing, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d engine operations have no operation id or no effect. Add each to engineOperations:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}

// An entry for an operation the document no longer holds would describe
// nothing, and the count above would not notice.
func TestOpenAPI_EveryEngineDescriptionNamesAnOperation(t *testing.T) {
	paths := engineDocWithEverything().Paths
	for _, e := range engineOperations {
		if paths[e.path][e.method] == nil {
			t.Errorf("engineOperations describes %s %s, which the document does not hold", strings.ToUpper(e.method), e.path)
		}
	}
}

func TestOpenAPI_EngineOperationIDsAreUnique(t *testing.T) {
	owner := map[string]string{}
	for path, methods := range engineDocWithEverything().Paths {
		for method, op := range methods {
			if op.OperationID == "" {
				continue
			}
			route := strings.ToUpper(method) + " " + path
			if first, taken := owner[op.OperationID]; taken {
				t.Errorf("%s and %s share the operation id %q", first, route, op.OperationID)
			}
			owner[op.OperationID] = route
		}
	}
}

func TestOpenAPI_EngineEffectsAreKnown(t *testing.T) {
	for path, methods := range engineDocWithEverything().Paths {
		for method, op := range methods {
			e, ok := op.Extensions[xEffect]
			if !ok {
				continue
			}
			if effect, isEffect := e.(core.Effect); !isEffect || !effect.Valid() {
				t.Errorf("%s %s carries effect %v, which is not one of the five", strings.ToUpper(method), path, e)
			}
		}
	}
}

// A content read changes nothing and a delete removes, and a model reading
// the document learns both before it calls either.
func TestOpenAPI_ContentFamilyIsDescribed(t *testing.T) {
	paths := engineDocWithEverything().Paths
	list := paths["/api/v1/content/{schema}"]["get"]
	require.NotNil(t, list)
	assert.Equal(t, "listContent", list.OperationID)
	assert.Equal(t, core.EffectRead, list.Extensions[xEffect])

	del := paths["/api/v1/content/{schema}/{id}"]["delete"]
	require.NotNil(t, del)
	assert.Equal(t, "deleteContent", del.OperationID)
	assert.Equal(t, core.EffectDelete, del.Extensions[xEffect])
}

// The list filters by field, and a caller that only reads the document has
// to learn the spelling from it.
func TestOpenAPI_ContentListNamesItsFilters(t *testing.T) {
	list := engineDocWithEverything().Paths["/api/v1/content/{schema}"]["get"]
	require.NotNil(t, list)
	var filters *openAPIParameter
	for i := range list.Parameters {
		if list.Parameters[i].Name == "filters" {
			filters = &list.Parameters[i]
		}
	}
	require.NotNil(t, filters, "the list documents its filters parameter")
	raw, err := json.Marshal(filters)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"style":"deepObject"`)
	assert.Contains(t, string(raw), `"explode":true`)
	assert.Contains(t, filters.Description, "filters[field]=value")
}

func widgetsDoc(routes ...core.RouteDecl) openAPIDoc {
	return buildOpenAPIDoc(nil, []plugin.PluginRoutes{{Name: "widgets", Routes: routes}}, openAPIOptions{})
}

func TestOpenAPI_RouteDocDescribesTheOperation(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	schema := map[string]any{
		"type":       "object",
		"required":   []string{"name"},
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
	}
	doc := widgetsDoc(core.RouteDecl{
		Method: "POST", Pattern: "/api/admin/widgets", Handler: ok, Group: core.GroupAdmin,
		Doc: &core.RouteDoc{
			OperationID:    "createWidget",
			Effect:         core.EffectWrite,
			RequestSchema:  schema,
			RequestExample: map[string]any{"name": "Front door"},
		},
	})

	op := doc.Paths["/api/admin/widgets"]["post"]
	require.NotNil(t, op)
	assert.Equal(t, "createWidget", op.OperationID)
	assert.Equal(t, core.EffectWrite, op.Extensions[xEffect])
	require.NotNil(t, op.RequestBody)
	assert.True(t, op.RequestBody.Required)
	body := op.RequestBody.Content["application/json"].Schema
	assert.Equal(t, []string{"name"}, body["required"])
	assert.Equal(t, map[string]any{"name": "Front door"}, body["example"])
	_, exampleLeaked := schema["example"]
	assert.False(t, exampleLeaked, "the route's own schema map is left as the plugin wrote it")
}

func TestOpenAPI_RequestSchemaAloneDocumentsABody(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	doc := widgetsDoc(core.RouteDecl{
		Method: "PUT", Pattern: "/api/admin/widgets/{id}", Handler: ok, Group: core.GroupAdmin,
		Doc: &core.RouteDoc{RequestSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}}},
	})
	op := doc.Paths["/api/admin/widgets/{id}"]["put"]
	require.NotNil(t, op)
	require.NotNil(t, op.RequestBody)
	assert.Contains(t, op.RequestBody.Content["application/json"].Schema, "properties")
}

func TestOpenAPI_UnknownEffectIsLeftOut(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	doc := widgetsDoc(core.RouteDecl{
		Method: "GET", Pattern: "/api/admin/widgets", Handler: ok, Group: core.GroupAdmin,
		Doc: &core.RouteDoc{OperationID: "listWidgets", Effect: "harmless"},
	})
	op := doc.Paths["/api/admin/widgets"]["get"]
	require.NotNil(t, op)
	assert.Equal(t, "listWidgets", op.OperationID)
	_, has := op.Extensions[xEffect]
	assert.False(t, has, "an effect that is not one of the five is not published")
}

// A plugin naming an id the engine or an earlier route holds is documented
// without one, so the document never names two operations alike.
func TestOpenAPI_TakenOperationIDIsLeftOff(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	doc := widgetsDoc(
		core.RouteDecl{Method: "GET", Pattern: "/api/admin/widgets", Handler: ok, Group: core.GroupAdmin,
			Doc: &core.RouteDoc{OperationID: "listWidgets", Effect: core.EffectRead}},
		core.RouteDecl{Method: "GET", Pattern: "/api/admin/widgets/archive", Handler: ok, Group: core.GroupAdmin,
			Doc: &core.RouteDoc{OperationID: "listWidgets", Effect: core.EffectRead}},
		core.RouteDecl{Method: "GET", Pattern: "/api/admin/widgets/content", Handler: ok, Group: core.GroupAdmin,
			Doc: &core.RouteDoc{OperationID: "listContent", Effect: core.EffectRead}},
	)
	assert.Equal(t, "listWidgets", doc.Paths["/api/admin/widgets"]["get"].OperationID)
	assert.Empty(t, doc.Paths["/api/admin/widgets/archive"]["get"].OperationID)
	assert.Empty(t, doc.Paths["/api/admin/widgets/content"]["get"].OperationID)
	assert.Equal(t, "listContent", doc.Paths["/api/v1/content/{schema}"]["get"].OperationID)
}
