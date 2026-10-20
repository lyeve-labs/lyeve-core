package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// oneRevision accepts snapshots and serves one stored revision for any id.
type oneRevision struct {
	core.RecordRevisionStore
	rev core.RecordRevision
}

func (oneRevision) SaveRecordRevision(context.Context, string, uuid.UUID, map[string]any, *uuid.UUID) error {
	return nil
}

func (o oneRevision) GetRecordRevision(context.Context, uuid.UUID) (core.RecordRevision, error) {
	return o.rev, nil
}

// An update and a restore change fields, never the publication status. A
// client writing back a body it read while the row was published would
// otherwise publish a row an editor has since unpublished, and restoring a
// revision taken while the row was published would publish a draft. The
// publish route still publishes.
func TestContentHandler_UpdateAndRestoreKeepTheStatus(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		id := fx.createStatus(t, "drafts", "draft", map[string]any{"title": "unreleased"})
		one := map[string]string{"schema": "drafts", "id": id}
		readerStatus := func() int {
			t.Helper()
			return fx.send(t, asReader(fx.ctx), http.MethodGet, "/api/v1/content/drafts/"+id, one, nil, fx.content.Get).Code
		}

		rr := fx.send(t, asEditor(fx.ctx), http.MethodPut, "/api/v1/content/drafts/"+id, one,
			map[string]any{"data": map[string]any{"title": "revised", "_status": "published"}}, fx.content.Update)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, "revised", dataOf(t, rr)["title"])
		assert.Equal(t, "draft", dataOf(t, rr)["_status"], "an update published the row")
		assert.Equal(t, http.StatusNotFound, readerStatus(), "a reader read the draft after an update")

		fx.content.revisions = fakeRevisionProvider{oneRevision{rev: core.RecordRevision{
			ID: uuid.New(), Data: map[string]any{"title": "as published", "_status": "published"},
		}}}
		revID := uuid.NewString()
		rr = fx.send(t, asEditor(fx.ctx), http.MethodPut, "/api/v1/content/drafts/"+id+"/revisions/"+revID+"/restore",
			map[string]string{"schema": "drafts", "id": id, "rev_id": revID}, nil, fx.content.RestoreRevision)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, "as published", dataOf(t, rr)["title"])
		assert.Equal(t, "draft", dataOf(t, rr)["_status"], "a restore published the row")
		assert.Equal(t, http.StatusNotFound, readerStatus(), "a reader read the draft after a restore")

		rr = fx.send(t, asEditor(fx.ctx), http.MethodPut, "/api/v1/content/drafts/"+id+"/publish", one, nil, fx.content.Publish)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		assert.Equal(t, http.StatusOK, readerStatus(), "the publish route did not publish")
	})
}
