package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A filter on a field the caller's mask hides answers which entries hold a
// guessed value, so it is refused rather than run. Before the fix the filter
// ran and the count of entries returned confirmed or denied each guess.
func TestContentHandler_FilterOnHiddenFieldIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		createdID(t, fx.create(t, "notes", map[string]any{
			"title": "secret title", "body": "hello",
		}))
		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"notes"}},
			mask:  map[string][]string{"notes": {"title"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		rr := call(t, r, http.MethodGet, "reader", "/api/v1/content/notes?filters[title]=secret%20title", nil)
		require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), "secret title")

		rr = call(t, r, http.MethodGet, "reader", "/api/v1/content/notes?filters[status]=open", nil)
		assert.Equal(t, http.StatusOK, rr.Code, "a filter on a readable field still runs: %s", rr.Body.String())
		assert.Contains(t, rr.Body.String(), "hello")
		assert.NotContains(t, rr.Body.String(), "secret title")
	})
}

func TestHiddenFields_NamesTheFieldAndItsRelationColumn(t *testing.T) {
	hidden := core.HiddenFields{"author": {}, "email": {}}
	assert.True(t, hidden.Names("email"))
	assert.True(t, hidden.Names("author"))
	assert.True(t, hidden.Names("author_id"))
	assert.False(t, hidden.Names("title"))
	assert.False(t, hidden.Names("title_id"))
	assert.False(t, core.HiddenFields(nil).Names("email"))
}
