package enginehost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// draftingEngine is the reference engine with a shaper on top, registered
// the way a plugin registers its engine. A document field keeps the document
// it is sent and gains the HTML drawn from it. An html field's text is
// refused when it is "refuse" and stored otherwise.
type draftingEngine struct {
	*schemaengine.Engine
}

func (draftingEngine) ShapeValues(_ context.Context, sc *core.Schema, data map[string]any) (map[string]any, []core.SchemaValidationError, error) {
	out := map[string]any{}
	for k, v := range data {
		out[k] = v
	}
	var failures []core.SchemaValidationError
	for _, f := range sc.Fields {
		v, ok := data[f.Name]
		if !ok || f.FieldType != "rich_text" {
			continue
		}
		if v == "refuse" {
			failures = append(failures, core.SchemaValidationError{Field: f.Name, Code: "rich_text_refused", Rule: "rich_text"})
		}
		if obj, isObj := v.(map[string]any); isObj && f.Format == core.RichTextFormatDocument {
			out[f.Name] = map[string]any{"document": obj["document"], "html": "<p>drawn</p>"}
		}
	}
	return out, failures, nil
}

// A plugin's write through the host passes the shaper of the registered
// engine. While the registered engine supplies none, a document value is
// refused as unavailable and an html value is stored as it was sent.
func TestEngineHost_ContentWriteUsesTheRegisteredShaper(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			var pool db.DB
			switch name {
			case "mysql":
				pool = testdb.MySQL(t)
			case "mssql":
				pool = testdb.MSSQL(t)
			default:
				pool = testdb.Postgres(t)
			}
			ctx := context.Background()
			h := NewHost(pool, pool.SQLDB(), nil, hooks.NewRegistry(), "test").(*engineHost)
			plain := schemaengine.New(h)
			require.NoError(t, h.RegisterSchemaEngine(plain))
			const schema = "shaped_posts"
			def, err := json.Marshal(core.Schema{
				Name: schema,
				Fields: []core.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "body", FieldType: "rich_text"},
					{Name: "doc", FieldType: "rich_text", Format: core.RichTextFormatDocument},
				},
			})
			require.NoError(t, err)
			require.NoError(t, plain.Apply(ctx, schema, def))
			t.Cleanup(func() { _ = plain.Delete(context.Background(), schema) })

			id := uuid.New()
			err = h.UpsertContent(ctx, schema, id, map[string]any{"title": "a", "doc": map[string]any{"document": map[string]any{"type": "doc"}}})
			require.ErrorIs(t, err, core.ErrNoContentValueShaper, "an engine with no shaper cannot store a document")
			require.NoError(t, h.UpsertContent(ctx, schema, id, map[string]any{"title": "a", "body": "<div>as sent</div>"}))
			rows, err := h.ContentPage(ctx, schema, uuid.Nil, 10)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, "<div>as sent</div>", rows[0]["body"], "an engine with no shaper stores an html value as sent")

			require.NoError(t, h.RegisterSchemaEngine(draftingEngine{plain}))
			require.NoError(t, h.UpsertContent(ctx, schema, id, map[string]any{
				"body": "<p>b</p>",
				"doc":  map[string]any{"document": map[string]any{"type": "doc"}, "html": "<script>sent</script>"},
			}))
			rows, err = h.ContentPage(ctx, schema, uuid.Nil, 10)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, "<p>b</p>", rows[0]["body"])
			assert.Equal(t, map[string]any{"document": map[string]any{"type": "doc"}, "html": "<p>drawn</p>"}, rows[0]["doc"])

			err = h.UpsertContent(ctx, schema, id, map[string]any{"body": "refuse"})
			require.ErrorIs(t, err, core.ErrValidation)
			var refused *core.ContentValueError
			require.True(t, errors.As(err, &refused), "%v", err)
			assert.Equal(t, "body", refused.Failures[0].Field)
		})
	}
}
