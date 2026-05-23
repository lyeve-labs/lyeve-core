package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// update sends one PUT to the content route and returns the response.
func (fx schemaContentFixture) update(t *testing.T, schema, id string, data map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": data})
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPut, "/api/v1/content/"+schema+"/"+id, strings.NewReader(string(body)))
	r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": schema, "id": id})
	rr := httptest.NewRecorder()
	fx.content.Update(rr, r)
	return rr
}

// The update route writes only the fields it is sent, so it validates only
// those, and a partial update that leaves out a required field is accepted.
func TestContentHandler_Update_ValidatesOnlyThePresentFields(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes", "surveys"}, func(t *testing.T, fx schemaContentFixture) {
		newNote := func(t *testing.T) string {
			t.Helper()
			return createdID(t, fx.create(t, "notes", map[string]any{
				"title": "First", "body": "first",
			}))
		}

		t.Run("a patch without a required field succeeds and keeps it", func(t *testing.T) {
			id := newNote(t)
			rr := fx.update(t, "notes", id, map[string]any{"status": "closed"})
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

			got := fx.get(t, "notes", id)
			require.Equal(t, http.StatusOK, got.Code, got.Body.String())
			data := dataOf(t, got)
			assert.Equal(t, "closed", data["status"])
			assert.Equal(t, "first", data["body"])
			assert.Equal(t, "First", data["title"])
		})

		t.Run("an explicit null for a required field is refused", func(t *testing.T) {
			id := newNote(t)
			rr := fx.update(t, "notes", id, map[string]any{"body": nil})
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), `"body"`)
			assert.Contains(t, rr.Body.String(), `"required"`)
			assert.Equal(t, "first", dataOf(t, fx.get(t, "notes", id))["body"])
		})

		t.Run("an empty string for a required field is refused", func(t *testing.T) {
			id := newNote(t)
			rr := fx.update(t, "notes", id, map[string]any{"body": ""})
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), `"required"`)
		})

		t.Run("a present field outside its enum is refused", func(t *testing.T) {
			id := newNote(t)
			rr := fx.update(t, "notes", id, map[string]any{"status": "deleted"})
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), `"enum"`)
			assert.Equal(t, "open", dataOf(t, fx.get(t, "notes", id))["status"])
		})

		t.Run("a present field of the wrong type is refused", func(t *testing.T) {
			id := newNote(t)
			rr := fx.update(t, "notes", id, map[string]any{"title": true})
			require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), `"field_type"`)
		})

		t.Run("a present unique field that collides is refused", func(t *testing.T) {
			taken := "taken-" + uuid.NewString()[:8]
			questions := []any{map[string]any{"prompt": "How did you start?", "kind": "text"}}
			createdID(t, fx.create(t, "surveys", map[string]any{"name": "A", "slug": taken, "questions": questions}))
			id := fx.createSurvey(t)
			rr := fx.update(t, "surveys", id, map[string]any{"slug": taken})
			assert.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
		})
	})
}
