package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// engineWithoutRules is a schema engine that supplies definitions and no
// content rules.
type engineWithoutRules struct{ core.SchemaEngine }

// A content write is stored only after the schema engine's rules pass it.
// With no rules to ask, the write is refused as unavailable and nothing is
// stored, whether the handler was given no rules at all or rules resolved
// from an engine that supplies none.
func TestContentHandler_WriteWithoutContentRulesIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"posts"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "posts", map[string]any{"title": "stored while the rules answered"}))

		for name, rules := range map[string]core.ContentRules{
			"no rules":                    nil,
			"an engine that has no rules": core.ContentRulesOf(engineWithoutRules{fx.eng}),
			"rules resolved to nothing":   core.LazyContentRules(func() core.ContentRules { return nil }),
		} {
			t.Run(name, func(t *testing.T) {
				h := *fx.content
				h.rules = rules
				w := fx
				w.content = &h

				rr := w.create(t, "posts", map[string]any{"title": "unchecked"})
				assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())

				rr = w.update(t, "posts", id, map[string]any{"title": "unchecked"})
				assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())

				body, err := json.Marshal(map[string]any{"items": []map[string]any{{"title": "unchecked"}}})
				require.NoError(t, err)
				r := httptest.NewRequest(http.MethodPost, "/api/v1/content/posts/bulk", strings.NewReader(string(body)))
				r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": "posts"})
				bulk := httptest.NewRecorder()
				w.content.BulkCreate(bulk, r)
				assert.Equal(t, http.StatusServiceUnavailable, bulk.Code, bulk.Body.String())
			})
		}

		var count int
		row, err := fx.pool.QueryRow(fx.ctx, "SELECT COUNT(*) FROM "+core.TableName("posts"))
		require.NoError(t, err)
		require.NoError(t, row.Scan(&count))
		assert.Equal(t, 1, count, "a refused write stored a row")
		got := fx.get(t, "posts", id)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		assert.Equal(t, "stored while the rules answered", dataOf(t, got)["title"])
	})
}
