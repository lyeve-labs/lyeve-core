package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Every route that serves an entry names its content type in the path, so
// the entry does not repeat it.
func TestContentHandler_EntryLeavesItsTypeOut(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		created := fx.create(t, "notes", map[string]any{"title": "first", "body": "hello"})
		id := createdID(t, created)
		fx.content.perms = fakeProvider{ruleChecker{reads: map[string][]string{"reader": {"notes"}}}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		bodies := []string{created.Body.String()}
		for _, path := range []string{"/api/v1/content/notes", "/api/v1/content/notes/cursor", "/api/v1/content/notes/" + id} {
			rr := call(t, r, http.MethodGet, "reader", path, nil)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", path, rr.Body.String())
			bodies = append(bodies, rr.Body.String())
		}
		for _, body := range bodies {
			assert.Contains(t, body, id)
			assert.NotContains(t, body, "schema_name")
		}
	})
}
