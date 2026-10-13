package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A belongs_to relation reaches a reader in two forms: the column it is
// stored in, and the related record under the field name when it is
// populated. A rule may name either form. The mask hid only the form the rule
// named, so a rule on the field served the column on every read, and a rule
// on the column served the populated record. Both forms are hidden, and
// refused as filters, whether the column has the default name or the one the
// relation's definition gives it.
func TestContentHandler_MaskHidesBothFormsOfARelation(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"bylines"}, func(t *testing.T, fx schemaContentFixture) {
		writer := createdID(t, fx.create(t, "writers", map[string]any{"name": "Ada"}))
		piece := createdID(t, fx.create(t, "pieces", map[string]any{
			"title": "On engines", "author": writer, "reviewer": writer,
		}))
		// One relation is hidden by its field name, the other by its column.
		fx.content.perms = fakeProvider{fakeChecker{allow: true, mask: []string{"author", "checked_by"}}}
		editor := asEditor(fx.ctx)
		forms := []string{"author", "author_id", "reviewer", "checked_by"}

		one := map[string]string{"schema": "pieces", "id": piece}
		rr := fx.send(t, editor, http.MethodGet, "/api/v1/content/pieces/"+piece+"?populate=author,reviewer", one, nil, fx.content.Get)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got := dataOf(t, rr)
		assert.Equal(t, "On engines", got["title"])
		for _, key := range forms {
			assert.NotContains(t, got, key, "a read served %s", key)
		}

		all := map[string]string{"schema": "pieces"}
		rr = fx.send(t, editor, http.MethodGet, "/api/v1/content/pieces?populate=author,reviewer", all, nil, fx.content.List)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var list []struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &list), rr.Body.String())
		require.Len(t, list, 1, rr.Body.String())
		for _, key := range forms {
			assert.NotContains(t, list[0].Data, key, "a list served %s", key)
		}

		for _, key := range forms {
			rr := fx.send(t, editor, http.MethodGet, "/api/v1/content/pieces?filters["+key+"]="+writer, all, nil, fx.content.List)
			assert.Equal(t, http.StatusForbidden, rr.Code, "filter on %s: %s", key, rr.Body.String())
		}

		// The superset is the rule's own: an unmasked caller still reads the
		// relation in both forms.
		fx.content.perms = fakeProvider{fakeChecker{allow: true}}
		rr = fx.send(t, editor, http.MethodGet, "/api/v1/content/pieces/"+piece+"?populate=author,reviewer", one, nil, fx.content.Get)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		got = dataOf(t, rr)
		for _, key := range forms {
			assert.Contains(t, got, key, "an unmasked read lost %s", key)
		}
	})
}
