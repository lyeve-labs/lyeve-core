package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Revision history belongs to a plugin. The engine writes a snapshot before
// every update and reads one back to restore it, and answers honestly when
// nothing keeps them: an empty list would say the record was never edited.

// memRevisions is the registered store as the handlers see it, holding rows
// in memory rather than in the plugin's table.
type memRevisions struct {
	mu   sync.Mutex
	rows []core.RecordRevision
}

func (m *memRevisions) SaveRecordRevision(_ context.Context, schemaName string, recordID uuid.UUID, data map[string]any, actor *uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, core.RecordRevision{
		ID: uuid.New(), SchemaName: schemaName, RecordID: recordID,
		Data: data, CreatedBy: actor, CreatedAt: time.Now(),
	})
	return nil
}

func (m *memRevisions) ListRecordRevisions(_ context.Context, schemaName string, recordID uuid.UUID) ([]core.RecordRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []core.RecordRevision
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.rows[i].SchemaName == schemaName && m.rows[i].RecordID == recordID {
			out = append(out, m.rows[i])
		}
	}
	return out, nil
}

func (m *memRevisions) GetRecordRevision(_ context.Context, id uuid.UUID) (core.RecordRevision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return core.RecordRevision{}, core.ErrRecordRevisionNotFound
}

// revisionProvider is the host as the content handler sees it: whatever the
// plugin registered last, nil included.
type revisionProvider struct{ s core.RecordRevisionStore }

func (p *revisionProvider) RecordRevisionStore() core.RecordRevisionStore { return p.s }

func (fx schemaContentFixture) listRevisions(t *testing.T, h *ContentHandler, schema, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/"+schema+"/"+id+"/revisions", nil)
	r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": schema, "id": id})
	rr := httptest.NewRecorder()
	h.ListRevisions(rr, r)
	return rr
}

func (fx schemaContentFixture) restoreRevision(t *testing.T, h *ContentHandler, schema, id, revID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/content/"+schema+"/"+id+"/revisions/"+revID+"/restore", nil)
	r = chiCtx(r.WithContext(fx.ctx), map[string]string{"schema": schema, "id": id, "rev_id": revID})
	rr := httptest.NewRecorder()
	h.RestoreRevision(rr, r)
	return rr
}

func TestContentHandler_Revisions_WithoutAPluginSayNotAvailable(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{
			"title": "First", "body": "first",
		}))

		// The handler carries no provider at all, which is the kernel-only
		// build: nothing ever called WithRecordRevisionStore.
		rr := fx.listRevisions(t, fx.content, "notes", id)
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code,
			"an empty list reads as a record nobody has edited, which is data loss wearing a 200")
		assert.NotContains(t, rr.Body.String(), "[]")

		rr = fx.restoreRevision(t, fx.content, "notes", id, uuid.New().String())
		assert.Equal(t, http.StatusServiceUnavailable, rr.Code)

		// A registered provider that answers nil is the same state: the
		// plugin stopped, or never started.
		h := *fx.content
		h.revisions = &revisionProvider{}
		assert.Equal(t, http.StatusServiceUnavailable, fx.listRevisions(t, &h, "notes", id).Code)

		// The write path is unaffected. An update with nowhere to record
		// history still succeeds, because the history is the secondary record.
		rr = fx.update(t, "notes", id, map[string]any{"body": "second"})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	})
}

func TestContentHandler_Revisions_WithAPluginRecordRestoreAndList(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{
			"title": "First", "body": "first",
		}))

		h := *fx.content
		h.revisions = &revisionProvider{s: &memRevisions{}}
		wired := fx

		// Route the update through the handler that can reach the store.
		wired.content = &h
		require.Equal(t, http.StatusOK, wired.update(t, "notes", id, map[string]any{"body": "second"}).Code)

		rr := fx.listRevisions(t, &h, "notes", id)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var revs []core.RecordRevision
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &revs))
		require.Len(t, revs, 1, "the update snapshots the state it replaced")
		assert.Equal(t, "first", revs[0].Data["body"])
		assert.Equal(t, "notes", revs[0].SchemaName)

		rr = fx.restoreRevision(t, &h, "notes", id, revs[0].ID.String())
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var out struct {
			Data map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(fx.get(t, "notes", id).Body.Bytes(), &out))
		assert.Equal(t, "first", out.Data["body"], "the restore wrote the snapshot back")

		rr = fx.listRevisions(t, &h, "notes", id)
		require.Equal(t, http.StatusOK, rr.Code)
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &revs))
		assert.Len(t, revs, 2, "the restore snapshots what it overwrote")
	})
}

func TestContentHandler_Revisions_UnknownRevisionIsNotFound(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{
			"title": "First", "body": "first",
		}))
		h := *fx.content
		h.revisions = &revisionProvider{s: &memRevisions{}}
		assert.Equal(t, http.StatusNotFound,
			fx.restoreRevision(t, &h, "notes", id, uuid.New().String()).Code)
	})
}

// refusingRevisions is a store whose plugin keeps some history behind a
// license: it lists nothing and refuses every read with the refusal that
// names what is missing.
type refusingRevisions struct{ memRevisions }

func (*refusingRevisions) GetRecordRevision(context.Context, uuid.UUID) (core.RecordRevision, error) {
	return core.RecordRevision{}, &core.NotGrantedError{Plugin: "widgets", Feature: "feature:widgets_history"}
}

func TestContentHandler_Revisions_RefusalNamesTheFeatureAndEmptyListIsAnArray(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{
			"title": "First", "body": "first",
		}))

		h := *fx.content
		h.revisions = &revisionProvider{s: &refusingRevisions{}}

		rr := fx.listRevisions(t, &h, "notes", id)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.JSONEq(t, "[]", rr.Body.String(), "a record with no history lists an empty array, not null")

		rr = fx.restoreRevision(t, &h, "notes", id, uuid.New().String())
		require.Equal(t, http.StatusPaymentRequired, rr.Code, rr.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "payment_required", body["error"])
		assert.Equal(t, "widgets", body["plugin"])
		assert.Equal(t, "feature:widgets_history", body["feature"])
	})
}
