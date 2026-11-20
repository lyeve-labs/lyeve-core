package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A content type served read-only over REST is written only by the routes of
// whatever owns it, so every write route on the Content API refuses it with
// the answer the generic update gets, and one that is off answers as absent.
// The definition changes after the rows exist, the way a plugin narrows its
// own type once its routes take over the writes.

// narrowREST sets schema's REST mode in the fixture's registry, which is the
// definition the content handler reads on every request.
func (fx schemaContentFixture) narrowREST(t *testing.T, schema string, mode core.TransportMode) {
	t.Helper()
	sc, err := fx.eng.GetByName(fx.ctx, schema)
	require.NoError(t, err)
	sc.Transports = core.SchemaTransports{core.TransportREST: mode}
}

func TestContentHandler_RestoreRevision_RefusesATypeNotWritableOverREST(t *testing.T) {
	for _, tc := range []struct {
		mode    core.TransportMode
		status  int
		message string
	}{
		{core.TransportReadOnly, http.StatusMethodNotAllowed, "content type is read-only over this transport"},
		{core.TransportOff, http.StatusNotFound, "content type not found"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
				id := createdID(t, fx.create(t, "notes", map[string]any{"title": "First", "body": "first"}))
				revs := &memRevisions{}
				h := *fx.content
				h.revisions = &revisionProvider{s: revs}
				wired := fx
				wired.content = &h
				require.Equal(t, http.StatusOK, wired.update(t, "notes", id, map[string]any{"body": "second"}).Code)
				require.Len(t, revs.rows, 1)

				fx.narrowREST(t, "notes", tc.mode)
				rr := fx.restoreRevision(t, &h, "notes", id, revs.rows[0].ID.String())
				assert.Equal(t, tc.status, rr.Code, rr.Body.String())
				assert.Contains(t, rr.Body.String(), tc.message)
				assert.Equal(t, rr.Code, wired.update(t, "notes", id, map[string]any{"body": "third"}).Code,
					"the restore and the generic update answer alike")

				fx.narrowREST(t, "notes", core.TransportReadWrite)
				assert.Equal(t, "second", dataOf(t, fx.get(t, "notes", id))["body"], "a refused restore wrote the revision back")
				assert.Len(t, revs.rows, 1, "a refused restore recorded the state it did not replace")
			})
		})
	}
}

func TestContentHandler_StatusAndRelationWrites_RefuseATypeReadOnlyOverREST(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"essays"}, func(t *testing.T, fx schemaContentFixture) {
		topic := fx.createStatus(t, "topics", "published", map[string]any{"name": "Go"})
		essay := fx.createStatus(t, "essays", "draft", map[string]any{"title": "On reading"})
		params := map[string]string{"schema": "essays", "id": essay}
		base := "/api/v1/content/essays/" + essay

		fx.narrowREST(t, "essays", core.TransportReadOnly)
		for _, tc := range []struct {
			name  string
			path  string
			body  any
			serve http.HandlerFunc
		}{
			{"publish", "/publish", nil, fx.content.Publish},
			{"unpublish", "/unpublish", nil, fx.content.Unpublish},
			{"relations", "/relations/topics", map[string]any{"ids": []string{topic}}, fx.content.SetRelations},
		} {
			p := map[string]string{"schema": "essays", "id": essay, "field": "topics"}
			rr := fx.send(t, fx.ctx, http.MethodPut, base+tc.path, p, tc.body, tc.serve)
			assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, tc.name+": "+rr.Body.String())
			assert.Contains(t, rr.Body.String(), "content type is read-only over this transport", tc.name)
		}

		fx.narrowREST(t, "essays", core.TransportReadWrite)
		assert.Equal(t, "draft", dataOf(t, fx.send(t, fx.ctx, http.MethodGet, base, params, nil, fx.content.Get))["_status"],
			"a refused publish changed the status")
		rr := fx.send(t, fx.ctx, http.MethodGet, base+"/relations/topics",
			map[string]string{"schema": "essays", "id": essay, "field": "topics"}, nil, fx.content.ListRelations)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var linked struct {
			Data []map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &linked), rr.Body.String())
		assert.Empty(t, linked.Data, "a refused relation write linked the topic")
	})
}
