package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func (fx schemaContentFixture) rowCount(t *testing.T, schema string) int {
	t.Helper()
	row, err := fx.pool.QueryRow(fx.ctx, "SELECT COUNT(*) FROM "+domain.TableName(schema))
	require.NoError(t, err)
	var n int
	require.NoError(t, row.Scan(&n))
	return n
}

// A value the shaper refuses is answered 422 in the errors shape every other
// content refusal uses, on a create and on an update, and nothing is stored.
func TestContentHandler_ShaperRefusalIsA422InTheErrorsShape(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"rich_text"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "rt_posts", map[string]any{"title": "kept", "body": "<p>kept</p>"}))
		fx.rules.unshapeable = map[string]core.SchemaValidationError{
			"body": {Field: "body", Code: "rich_text_too_large", Rule: "rich_text", Message: "The text is too large to store."},
		}

		assertRefused := func(t *testing.T, code int, raw []byte) {
			t.Helper()
			require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
			var out struct {
				Errors []struct {
					Field   string `json:"field"`
					Message string `json:"message"`
					Code    string `json:"code"`
					Rule    string `json:"rule"`
				} `json:"errors"`
			}
			require.NoError(t, json.Unmarshal(raw, &out), string(raw))
			require.Len(t, out.Errors, 1, string(raw))
			assert.Equal(t, "body", out.Errors[0].Field)
			assert.Equal(t, "rich_text_too_large", out.Errors[0].Code)
			assert.Equal(t, "rich_text", out.Errors[0].Rule)
			assert.Equal(t, "The text is too large to store.", out.Errors[0].Message)
		}

		rr := fx.create(t, "rt_posts", map[string]any{"title": "new", "body": "<p>too much</p>"})
		assertRefused(t, rr.Code, rr.Body.Bytes())
		rr = fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/rt_posts/"+id,
			map[string]string{"schema": "rt_posts", "id": id},
			map[string]any{"data": map[string]any{"body": "<p>too much</p>"}}, fx.content.Update)
		assertRefused(t, rr.Code, rr.Body.Bytes())

		assert.Equal(t, 1, fx.rowCount(t, "rt_posts"))
		assert.Equal(t, "<p>kept</p>", dataOf(t, fx.get(t, "rt_posts", id))["body"])
	})
}

// An install whose engine supplies no shaper answers 503 for a write naming a
// rich text field, an html one as well as a document, and stores nothing, as
// it does with no schema engine. A write naming no rich text field is stored.
func TestContentHandler_RichTextWithNoShaperIs503(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"rich_text"}, func(t *testing.T, fx schemaContentFixture) {
		src := fx.eng.SchemaSource()
		fx.content = &ContentHandler{store: newTestContentStore(fx.pool, src), schemas: src, rules: fx.rules, hooks: hooks.NewRegistry()}

		rr := fx.create(t, "rt_posts", map[string]any{"title": "rich", "doc": map[string]any{"document": map[string]any{"type": "doc"}}})
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
		assert.Equal(t, 0, fx.rowCount(t, "rt_posts"))

		rr = fx.create(t, "rt_posts", map[string]any{"title": "html", "body": "<div>x</div>"})
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
		assert.Equal(t, 0, fx.rowCount(t, "rt_posts"))

		id := createdID(t, fx.create(t, "rt_posts", map[string]any{"title": "plain"}))
		rr = fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/rt_posts/"+id,
			map[string]string{"schema": "rt_posts", "id": id},
			map[string]any{"data": map[string]any{"doc": map[string]any{"document": map[string]any{"type": "doc"}}}}, fx.content.Update)
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
		assert.Nil(t, dataOf(t, fx.get(t, "rt_posts", id))["doc"])
	})
}

// A document field answers its object on the write and on every read, and an
// html field answers the text it holds.
func TestContentHandler_DocumentFieldReadsAsAnObject(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"rich_text"}, func(t *testing.T, fx schemaContentFixture) {
		doc := map[string]any{
			"document": map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph"}}},
			"html":     "<p></p>",
		}
		body := `<p>{"looks":"like json"}</p>`
		rr := fx.create(t, "rt_posts", map[string]any{"title": "t", "body": body, "doc": doc})
		id := createdID(t, rr)
		assert.Equal(t, doc, dataOf(t, rr)["doc"])

		got := dataOf(t, fx.get(t, "rt_posts", id))
		assert.Equal(t, doc, got["doc"])
		assert.Equal(t, body, got["body"])

		rr = fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/rt_posts", map[string]string{"schema": "rt_posts"}, nil, fx.content.List)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var items []struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &items), rr.Body.String())
		require.Len(t, items, 1)
		assert.Equal(t, doc, items[0].Data["doc"])
		assert.Equal(t, body, items[0].Data["body"])
	})
}
