package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A to-many relation keeps its rows in the target table or a pivot, so the
// list has no column of its own to compare. A filter naming one is an
// unknown key, answered as any other name the table lacks.
func TestContentHandler_ListFilterNamingAToManyRelationIsAnUnknownKey(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		list := func(query string) *http.Response {
			rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/shelves?"+query,
				map[string]string{"schema": "shelves"}, nil, fx.content.List)
			return rr.Result()
		}
		unknown := list("filters[nosuch]=" + shelf)
		require.Equal(t, http.StatusBadRequest, unknown.StatusCode)
		for _, field := range []string{"books", "pages"} {
			got := list("filters[" + field + "]=" + shelf)
			assert.Equal(t, http.StatusBadRequest, got.StatusCode, "filters[%s]", field)
		}
	})
}
