package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// populateTooLargeHint is the part of the refusal that tells a caller what
// to narrow.
const populateTooLargeHint = "fewer relations, a smaller depth or a smaller page"

// listBooks lists the shelves set's books through the content route.
func (fx schemaContentFixture) listBooks(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/books?"+query,
		map[string]string{"schema": "books"}, nil, fx.content.List)
}

// getShelf reads one shelf through the content route.
func (fx schemaContentFixture) getShelf(t *testing.T, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/shelves/"+id+"?"+query,
		map[string]string{"schema": "shelves", "id": id}, nil, fx.content.Get)
}

// Sixty books on one shelf, read four levels deep, serve the shelf under
// each book, the sixty books under each shelf, and so on: 223,260 related
// entries from 122 rows. The read is refused rather than served whole or cut.
func TestContentHandler_PopulateFanOutPastTheBoundIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		for i := range 60 {
			createdID(t, fx.create(t, "books", map[string]any{"title": fmt.Sprintf("book %d", i), "shelves": shelf}))
		}

		rr := fx.listBooks(t, "limit=60&depth=4")
		require.Equal(t, http.StatusBadRequest, rr.Code, "body of %d bytes", rr.Body.Len())
		assert.Contains(t, rr.Body.String(), populateTooLargeHint)

		rr = fx.getShelf(t, shelf, "depth=4")
		require.Equal(t, http.StatusBadRequest, rr.Code, "body of %d bytes", rr.Body.Len())
		assert.Contains(t, rr.Body.String(), populateTooLargeHint)
	})
}

// A has_many with more children than the walk may read is refused, and the
// query reads at most one row past the budget to learn it.
func TestContentHandler_PopulateHasManyPastTheBudgetIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		const books = 5001
		for start := 0; start < books; start += 1000 {
			batch := make([]map[string]any, 0, 1000)
			for i := start; i < min(start+1000, books); i++ {
				batch = append(batch, map[string]any{"title": fmt.Sprintf("book %d", i), "shelves": shelf})
			}
			_, err := fx.content.store.BulkInsert(fx.ctx, "books", batch)
			require.NoError(t, err)
		}

		rr := fx.getShelf(t, shelf, "populate=books")
		require.Equal(t, http.StatusBadRequest, rr.Code, "body of %d bytes", rr.Body.Len())
		assert.Contains(t, rr.Body.String(), populateTooLargeHint)
	})
}

// depth reads as an integer from 0 to 10, and anything else is refused
// rather than ignored or cut to 10.
func TestContentHandler_PopulateDepthOutsideTheRangeIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		createdID(t, fx.create(t, "books", map[string]any{"title": "b", "shelves": shelf}))

		for _, depth := range []string{"11", "abc", "-1", "1.5", "+2"} {
			rr := fx.listBooks(t, "depth="+depth)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "list depth=%s: %s", depth, rr.Body.String())
			rr = fx.getShelf(t, shelf, "depth="+depth)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "get depth=%s: %s", depth, rr.Body.String())
		}
		for _, depth := range []string{"0", "10"} {
			rr := fx.listBooks(t, "depth="+depth)
			assert.Equal(t, http.StatusOK, rr.Code, "list depth=%s: %s", depth, rr.Body.String())
		}
	})
}

// A relation the database fails to read answers 503, not an empty relation
// that reads as a shelf with no books.
func TestContentHandler_PopulateReadFailureAnswers503(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		createdID(t, fx.create(t, "books", map[string]any{"title": "b", "shelves": shelf}))
		d := dialect.Must(fx.pool.Engine())
		_, err := fx.pool.Exec(fx.ctx, "ALTER TABLE "+domain.TableName("books")+" DROP COLUMN "+d.QuoteIdentifier("title"))
		require.NoError(t, err)

		rr := fx.getShelf(t, shelf, "populate=books")
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), "title", "driver text reached the response")
	})
}

// A walk inside the bounds serves what it always served: every relation at
// every level it reaches, a to-one as a record and a to-many as a list.
func TestContentHandler_PopulateInsideTheBoundsKeepsItsBody(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		var books, pages []string
		for i := range 2 {
			books = append(books, createdID(t, fx.create(t, "books", map[string]any{"title": fmt.Sprintf("book %d", i), "shelves": shelf})))
			pages = append(pages, createdID(t, fx.create(t, "pages", map[string]any{"title": fmt.Sprintf("page %d", i)})))
		}
		rr := fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/shelves/"+shelf+"/relations/pages",
			map[string]string{"schema": "shelves", "id": shelf, "field": "pages"},
			map[string]any{"ids": pages}, fx.content.SetRelations)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		type entry struct {
			ID   string         `json:"id"`
			Data map[string]any `json:"data"`
		}
		ids := func(v any) []string {
			list, ok := v.([]any)
			require.True(t, ok, "%T is no list", v)
			var out []string
			for _, item := range list {
				out = append(out, item.(map[string]any)["id"].(string))
			}
			slices.Sort(out)
			return out
		}
		slices.Sort(books)
		slices.Sort(pages)

		rr = fx.listBooks(t, "limit=10&depth=2")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var listed []entry
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &listed))
		require.Len(t, listed, 2)
		for _, b := range listed {
			s, ok := b.Data["shelves"].(map[string]any)
			require.True(t, ok, "book %s has no populated shelf: %v", b.ID, b.Data)
			assert.Equal(t, shelf, s["id"])
			assert.Equal(t, "s", s["name"])
			assert.Equal(t, books, ids(s["books"]))
			assert.Equal(t, pages, ids(s["pages"]))
		}

		rr = fx.getShelf(t, shelf, "populate=books,pages")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var got entry
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
		assert.Equal(t, books, ids(got.Data["books"]))
		assert.Equal(t, pages, ids(got.Data["pages"]))
	})
}

// A relation into a content type that was deleted stays declared on the type
// that pointed at it. Its read leaves that relation out, as the delivery
// routes do, rather than failing every read of the type with a wildcard.
func TestContentHandler_PopulateIntoADeletedTypeIsLeftOut(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		book := createdID(t, fx.create(t, "books", map[string]any{"title": "b", "shelves": shelf}))
		require.NoError(t, fx.eng.Delete(fx.ctx, "shelves"))

		for _, query := range []string{"populate=shelves", "populate=*", "depth=2"} {
			rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/books/"+book+"?"+query,
				map[string]string{"schema": "books", "id": book}, nil, fx.content.Get)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", query, rr.Body.String())
			var got struct {
				Data map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
			assert.Equal(t, "b", got.Data["title"], query)
			_, populated := got.Data["shelves"].(map[string]any)
			assert.False(t, populated, "%s served the deleted type's record: %v", query, got.Data)

			rr = fx.listBooks(t, query)
			require.Equal(t, http.StatusOK, rr.Code, "list %s: %s", query, rr.Body.String())
		}
	})
}
