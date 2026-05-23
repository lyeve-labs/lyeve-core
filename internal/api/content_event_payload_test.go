package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
)

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// readDocument is the document as GET returns it, with the id the event
// builder stamps beside its fields.
func readDocument(t *testing.T, fx schemaContentFixture, schema, id string) map[string]any {
	t.Helper()
	got := fx.get(t, schema, id)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	data := dataOf(t, got)
	data["id"] = id
	return data
}

// assertSameReference checks a relation column against the id it was
// written with. SQL Server renders a uniqueidentifier in upper case, in the
// event and in GET alike, so the comparison folds case.
func assertSameReference(t *testing.T, want string, got any) {
	t.Helper()
	s, ok := got.(string)
	require.True(t, ok, "reference is %T, want string", got)
	assert.True(t, strings.EqualFold(want, s), "reference %q, want %q", s, want)
}

// A flow reads the event the way it reads the document: trigger.data.survey_id
// on a response, trigger.data.id as a string. The payload is the document
// as a read returns it, not the caller's map with the row id added, so the
// relation sits under its column name and the id is a string.
func TestContentHandler_AfterCreatePayloadIsTheReadDocument(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"surveys"}, func(t *testing.T, fx schemaContentFixture) {
		var events []hooks.Event
		fx.content.hooks.Register("survey_responses", hooks.AfterCreate, func(_ context.Context, e hooks.Event) error {
			events = append(events, e)
			return nil
		})
		fx.content.hooks.Register("survey_responses", hooks.AfterUpdate, func(_ context.Context, e hooks.Event) error {
			events = append(events, e)
			return nil
		})

		surveyID := fx.createSurvey(t)
		id := createdID(t, fx.create(t, "survey_responses", map[string]any{
			"survey": surveyID, "data": map[string]any{"answer": "by reading"},
		}))
		require.Len(t, events, 1, "one after_create")
		ev := events[0]

		assert.Equal(t, id, ev.RecordID)
		assert.Equal(t, id, ev.Data["id"], "data.id is the string record_id, not the UUID bytes")
		assertSameReference(t, surveyID, ev.Data["survey_id"])
		_, byField := ev.Data["survey"]
		assert.False(t, byField, "the relation is not under its field name")
		assert.Equal(t, "new", ev.Data["status"], "the default the caller omitted is in the payload")
		doc := readDocument(t, fx, "survey_responses", id)
		assert.Equal(t, sortedKeys(doc), sortedKeys(ev.Data))
		assert.Equal(t, doc["survey_id"], ev.Data["survey_id"], "the payload and GET agree on the reference")

		// An update publishes the same document, with the row before it.
		body, err := json.Marshal(map[string]any{"data": map[string]any{
			"survey": surveyID, "data": map[string]any{"answer": "by reading"}, "status": "done",
		}})
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPut, "/api/v1/content/survey_responses/"+id, strings.NewReader(string(body)))
		r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": "survey_responses", "id": id})
		rr := httptest.NewRecorder()
		fx.content.Update(rr, r)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.Len(t, events, 2, "one after_update")
		up := events[1]
		assert.Equal(t, id, up.Data["id"])
		assertSameReference(t, surveyID, up.Data["survey_id"])
		assert.Equal(t, "done", up.Data["status"])
		assert.Equal(t, sortedKeys(readDocument(t, fx, "survey_responses", id)), sortedKeys(up.Data))
		assert.Equal(t, "new", up.OldData["status"])
	})
}
