package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// schemaContentFixture is a set of schemas applied through a supplied schema
// engine, and the content handler that serves their tables, on one dialect.
type schemaContentFixture struct {
	ctx     context.Context
	content *ContentHandler
}

// contentDefaultSchemas are the content models the tests below post, spelled
// as the bodies POST /api/admin/schemas accepts: a required field with a
// default, a boolean with a default and a relation between two schemas.
var contentDefaultSchemas = map[string][]string{
	"drafts": {`{
		"name": "drafts",
		"display_name": "Drafts",
		"with_created_at": true,
		"with_updated_at": true,
		"with_draft_publish": true,
		"fields": [
			{"name": "title", "field_type": "text", "required": true}
		]
	}`},
	"notes": {`{
		"name": "notes",
		"display_name": "Notes",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "title", "field_type": "text", "required": true, "indexed": true},
			{"name": "body", "field_type": "text", "required": true},
			{"name": "status", "field_type": "text", "required": true, "indexed": true, "default": "open",
			 "validation": [{"rule": "enum", "params": {"values": ["open", "closed"]}}]}
		]
	}`},
	"surveys": {`{
		"name": "surveys",
		"display_name": "Surveys",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "name", "field_type": "text", "required": true},
			{"name": "slug", "field_type": "text", "required": true, "unique": true},
			{"name": "questions", "field_type": "json", "required": true},
			{"name": "active", "field_type": "boolean", "default": true}
		]
	}`, `{
		"name": "survey_responses",
		"display_name": "Survey responses",
		"with_created_at": true,
		"fields": [
			{"name": "survey", "field_type": "relation", "relation_to": "surveys", "relation_type": "belongs_to", "required": true},
			{"name": "data", "field_type": "json", "required": true},
			{"name": "status", "field_type": "text", "required": true, "indexed": true, "default": "new",
			 "validation": [{"rule": "enum", "params": {"values": ["new", "done"]}}]}
		]
	}`},
	// Two belongs_to hops, staff to offices to regions, for population that
	// reaches a schema the request never named.
	"chain": {`{
		"name": "regions",
		"display_name": "Regions",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "name", "field_type": "text", "required": true},
			{"name": "secret", "field_type": "text"}
		]
	}`, `{
		"name": "offices",
		"display_name": "Offices",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "name", "field_type": "text", "required": true},
			{"name": "internal", "field_type": "text"},
			{"name": "region", "field_type": "relation", "relation_to": "regions", "relation_type": "belongs_to"}
		]
	}`, `{
		"name": "staff",
		"display_name": "Staff",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "name", "field_type": "text", "required": true},
			{"name": "office", "field_type": "relation", "relation_to": "offices", "relation_type": "belongs_to"}
		]
	}`},
	// A many_to_many relation, for the relation routes. labels carries a
	// field the API key tests mask.
	"articles": {`{
		"name": "labels",
		"display_name": "Labels",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "name", "field_type": "text", "required": true},
			{"name": "secret", "field_type": "text"}
		]
	}`, `{
		"name": "articles",
		"display_name": "Articles",
		"with_created_at": true,
		"with_updated_at": true,
		"fields": [
			{"name": "title", "field_type": "text", "required": true},
			{"name": "internal", "field_type": "text"},
			{"name": "tags", "field_type": "relation", "relation_to": "labels", "relation_type": "many_to_many"}
		]
	}`},
}

// forEachDialectWithSchemas posts the named schema sets on every dialect the
// run tests and hands the content handler over them to fn.
func forEachDialectWithSchemas(t *testing.T, setIDs []string, fn func(t *testing.T, fx schemaContentFixture)) {
	t.Helper()
	dialects := []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", testdb.Postgres},
		{"mysql", testdb.MySQL},
		{"mssql", testdb.MSSQL},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.pool(t)
			ctx := asSuperAdmin(tenant.WithID(context.Background(), "t1"))

			// The content path needs tables and a registry, and this module
			// builds neither. The supplied engine is the one this repository keeps for exactly this, and
			// it imports nothing but pkg/core, so a test that drives content
			// proves the same extension point an install depends on.
			eng := schemaengine.New(testhost.New(pool))
			for _, id := range setIDs {
				bodies, ok := contentDefaultSchemas[id]
				require.True(t, ok, id)
				var names []string
				for _, body := range bodies {
					var sc domain.Schema
					require.NoError(t, json.Unmarshal([]byte(body), &sc))
					names = append(names, sc.Name)
				}
				t.Cleanup(func() {
					// Children first: a response's foreign key holds the
					// surveys table in place until its own is gone, and a
					// pivot holds both sides of its relation.
					if id == "articles" {
						_, _ = pool.Exec(context.Background(), "DROP TABLE "+core.PivotTableName("articles", "labels"))
					}
					for i := len(names) - 1; i >= 0; i-- {
						_, _ = pool.Exec(context.Background(), "DROP TABLE "+domain.TableName(names[i]))
					}
				})
				for i, body := range bodies {
					require.NoError(t, eng.Apply(ctx, names[i], json.RawMessage(body)), names[i])
				}
			}
			src := eng.SchemaSource()
			fn(t, schemaContentFixture{
				ctx:     ctx,
				content: &ContentHandler{store: db.NewContentStore(pool, src), schemas: src, hooks: hooks.NewRegistry()},
			})
		})
	}
}

// create posts one document to the content route and returns the response.
func (fx schemaContentFixture) create(t *testing.T, schema string, data map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": data})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/content/"+schema, strings.NewReader(string(body)))
	r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": schema})
	rr := httptest.NewRecorder()
	fx.content.Create(rr, r)
	return rr
}

// get reads one document back through the content route.
func (fx schemaContentFixture) get(t *testing.T, schema, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/"+schema+"/"+id, nil)
	r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": schema, "id": id})
	rr := httptest.NewRecorder()
	fx.content.Get(rr, r)
	return rr
}

// createdID decodes the id of a 201 response.
func createdID(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusCreated, rr.Code, rr.Body.String())
	var out struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.NotEmpty(t, out.ID)
	_, err := uuid.Parse(out.ID)
	require.NoError(t, err, "id %q", out.ID)
	return out.ID
}

// createSurvey makes the survey a response has to point at.
func (fx schemaContentFixture) createSurvey(t *testing.T) string {
	t.Helper()
	return createdID(t, fx.create(t, "surveys", map[string]any{
		"name":      "Onboarding",
		"slug":      "onboarding-" + uuid.NewString()[:8],
		"questions": []any{map[string]any{"prompt": "How did you start?", "kind": "text"}},
	}))
}

func dataOf(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return out.Data
}

// Both sets declare status as required with a default. The default is
// applied before validation, so a note or a response posted without a
// status is created with the default on every dialect.
func TestContentHandler_Create_DefaultSatisfiesRequired(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes", "surveys"}, func(t *testing.T, fx schemaContentFixture) {
		t.Run("a note without status starts open", func(t *testing.T) {
			rr := fx.create(t, "notes", map[string]any{
				"title": "First", "body": "first",
			})
			id := createdID(t, rr)
			assert.Equal(t, "open", dataOf(t, rr)["status"])

			got := fx.get(t, "notes", id)
			require.Equal(t, http.StatusOK, got.Code, got.Body.String())
			assert.Equal(t, "open", dataOf(t, got)["status"])
		})

		t.Run("a response without status starts new", func(t *testing.T) {
			surveyID := fx.createSurvey(t)
			rr := fx.create(t, "survey_responses", map[string]any{
				"survey": surveyID, "data": map[string]any{"answer": "by reading"},
			})
			id := createdID(t, rr)
			assert.Equal(t, "new", dataOf(t, rr)["status"])

			got := fx.get(t, "survey_responses", id)
			require.Equal(t, http.StatusOK, got.Code, got.Body.String())
			assert.Equal(t, "new", dataOf(t, got)["status"])
		})

		t.Run("a status the caller sends wins over the default", func(t *testing.T) {
			rr := fx.create(t, "notes", map[string]any{
				"title": "Second", "body": "second", "status": "closed",
			})
			createdID(t, rr)
			assert.Equal(t, "closed", dataOf(t, rr)["status"])
		})

		t.Run("a required field with no default is still refused", func(t *testing.T) {
			rr := fx.create(t, "notes", map[string]any{"title": "Untitled"})
			assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), `"body"`)
		})
	})
}

// An update that omits a field leaves it alone. Filling the default in would
// switch a survey back on with every edit of its name.
func TestContentHandler_Update_OmittedFieldIsNotDefaulted(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"surveys"}, func(t *testing.T, fx schemaContentFixture) {
		slug := "onboarding-" + uuid.NewString()[:8]
		questions := []any{map[string]any{"prompt": "How did you start?", "kind": "text"}}
		id := createdID(t, fx.create(t, "surveys", map[string]any{
			"name": "Onboarding", "slug": slug, "questions": questions, "active": false,
		}))

		body, err := json.Marshal(map[string]any{"data": map[string]any{
			"name": "Onboarding survey", "slug": slug, "questions": questions,
		}})
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPut, "/api/v1/content/surveys/"+id, strings.NewReader(string(body)))
		r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": "surveys", "id": id})
		rr := httptest.NewRecorder()
		fx.content.Update(rr, r)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		got := fx.get(t, "surveys", id)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		data := dataOf(t, got)
		assert.Equal(t, "Onboarding survey", data["name"])
		// A boolean reads back as false or as 0, by dialect. Either is off.
		assert.Contains(t, []any{false, float64(0)}, data["active"], "active was reset to its default")
	})
}
