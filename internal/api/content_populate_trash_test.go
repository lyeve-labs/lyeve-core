package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A trashed entry stays out of its parent's has_many and many_to_many
// populate, as it stays out of a belongs_to populate, a list and a read by
// id. Moving an entry to the trash leaves its status as it was, so the
// published-only rule does not catch it.
func TestContentHandler_PopulateLeavesOutTrashedEntries(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		createdID(t, fx.create(t, "books", map[string]any{"title": "live book", "shelves": shelf}))
		trashedBook := createdID(t, fx.create(t, "books", map[string]any{"title": "trashed book", "shelves": shelf}))
		livePage := createdID(t, fx.create(t, "pages", map[string]any{"title": "live page"}))
		trashedPage := createdID(t, fx.create(t, "pages", map[string]any{"title": "trashed page"}))
		rr := fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/shelves/"+shelf+"/relations/pages",
			map[string]string{"schema": "shelves", "id": shelf, "field": "pages"},
			map[string]any{"ids": []string{livePage, trashedPage}}, fx.content.SetRelations)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		for schema, id := range map[string]string{"books": trashedBook, "pages": trashedPage} {
			rr := fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/"+schema+"/"+id,
				map[string]string{"schema": schema, "id": id}, nil, fx.content.Delete)
			require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		}

		rr = fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/shelves/"+shelf+"?populate=books,pages",
			map[string]string{"schema": "shelves", "id": shelf}, nil, fx.content.Get)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		body := rr.Body.String()
		assert.Contains(t, body, "live book")
		assert.Contains(t, body, "live page")
		assert.NotContains(t, body, "trashed book", "a trashed has_many child was populated")
		assert.NotContains(t, body, "trashed page", "a trashed many_to_many target was populated")
	})
}
