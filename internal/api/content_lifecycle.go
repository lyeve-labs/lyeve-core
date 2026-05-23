package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// actorID returns the authenticated user's ID for revision attribution, or nil
// when the caller is unauthenticated or authenticated by API key (whose subject
// is not a sys_users row).
func actorID(r *http.Request) *uuid.UUID {
	return actorIDFromCtx(r.Context())
}

// actorIDFromCtx is actorID for a caller that holds only the request's
// context, such as a configuration section applied on a plugin's route.
func actorIDFromCtx(ctx context.Context) *uuid.UUID {
	claims := core.GetClaims(ctx)
	if claims == nil || claims.IsAPIKey {
		return nil
	}
	id, err := uuid.Parse(claims.UserID)
	if err != nil {
		return nil
	}
	return &id
}

// Publish transitions a content record to _status='published'.
// PUT /api/v1/content/{schema}/{id}/publish
func (h *ContentHandler) Publish(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.checkPermission(w, r, schemaName, "update") {
		return
	}
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	h.setStatus(w, r, schemaName, id, "published", "failed to publish content")
}

// Unpublish transitions a content record to _status='draft'.
// PUT /api/v1/content/{schema}/{id}/unpublish
func (h *ContentHandler) Unpublish(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.checkPermission(w, r, schemaName, "update") {
		return
	}
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	h.setStatus(w, r, schemaName, id, "draft", "failed to unpublish content")
}

// setStatus moves a record to status and fires the after-update event a
// subscriber would see for any other write to it, so a search index, a
// webhook or a response cache hears that an entry was published or withdrawn.
// The status write has committed before the event fires, so a read failure
// around it is logged and the answer is still 204.
func (h *ContentHandler) setStatus(w http.ResponseWriter, r *http.Request, schemaName string, id uuid.UUID, status, failure string) {
	var before map[string]any
	if existing, err := h.store.GetByID(r.Context(), schemaName, id); err == nil && existing != nil {
		before = existing.Data
	}
	if err := h.store.SetStatus(r.Context(), schemaName, id, status); err != nil {
		httpx.ErrorReq(w, r, httpStatus(err), failure)
		return
	}
	if after, err := h.store.GetByID(r.Context(), schemaName, id); err == nil && after != nil {
		h.publishAfter(r, core.AfterUpdate, schemaName, id.String(), before, after.Data)
	} else if err != nil {
		slog.WarnContext(r.Context(), "content: read after status change failed", "schema", schemaName, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// revisionError answers a revision store failure. A refusal that names what
// the license lacks is answered with the 402 body a plugin's own route would
// send, and anything else with its status and the static message.
func revisionError(w http.ResponseWriter, r *http.Request, err error, failure string) {
	var refused *core.NotGrantedError
	if errors.As(err, &refused) {
		httpx.PaymentRequired(w, refused.Plugin, refused.Feature, "")
		return
	}
	httpx.ErrorReq(w, r, httpStatus(err), failure)
}

// ListRevisions returns the revision history for a content record.
// GET /api/v1/content/{schema}/{id}/revisions
func (h *ContentHandler) ListRevisions(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.checkPermission(w, r, schemaName, "read") {
		return
	}
	mask, maskOK := h.maskFor(w, r, schemaName)
	if !maskOK {
		return
	}
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	store, ok := h.revisionStore(w, r)
	if !ok {
		return
	}
	revisions, err := store.ListRecordRevisions(r.Context(), schemaName, id)
	if err != nil {
		revisionError(w, r, err, "failed to list revisions")
		return
	}
	if revisions == nil {
		revisions = []core.RecordRevision{}
	}
	for i := range revisions {
		revisions[i].Data = mask(revisions[i].Data)
	}
	respond(w, http.StatusOK, revisions)
}

// RestoreRevision replaces current content data with a historical revision snapshot.
// PUT /api/v1/content/{schema}/{id}/revisions/{rev_id}/restore
func (h *ContentHandler) RestoreRevision(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.checkPermission(w, r, schemaName, "update") {
		return
	}
	mask, maskOK := h.maskFor(w, r, schemaName)
	if !maskOK {
		return
	}
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	revID, err := reqparse.ParseUUID(r, "rev_id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid rev_id")
		return
	}
	store, ok := h.revisionStore(w, r)
	if !ok {
		return
	}
	rev, err := store.GetRecordRevision(r.Context(), revID)
	if err != nil {
		revisionError(w, r, err, "failed to get revision")
		return
	}
	// The restore needs the record's current version to write against, so a
	// read failure here is the caller's answer.
	existing, err := h.store.GetByID(r.Context(), schemaName, id)
	if err != nil {
		httpx.ErrorReq(w, r, httpStatus(err), "failed to get content for restore")
		return
	}
	h.snapshot(r, schemaName, id, existing.Data)
	item, err := h.store.Update(r.Context(), schemaName, id, rev.Data, existing.UpdatedAt)
	if err != nil {
		httpx.ErrorReq(w, r, httpStatus(err), "failed to restore revision")
		return
	}
	item.Data = mask(item.Data)
	respond(w, http.StatusOK, item)
}

// Stream sends a Server-Sent Events stream of content change events for a schema.
// GET /api/v1/content/{schema}/stream
//
// The hooks it registers fire for every write to the schema, in every tenant,
// because a generated table is shared and keyed by its tenant column. So the
// stream keeps only the events of the caller's own tenant, and each payload
// goes through the field mask and the after-response hooks a read applies,
// so the stream serves no field that GET would withhold from this caller.
func (h *ContentHandler) Stream(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "list") {
		return
	}
	if !h.checkPermission(w, r, schemaName, "read") {
		return
	}
	mask, maskOK := h.maskFor(w, r, schemaName)
	if !maskOK {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	type change struct {
		event string
		data  map[string]any
	}
	callerTenant := core.TenantIDFromCtx(r.Context())
	ch := make(chan change, 16)

	// The hook runs on the writer's goroutine, so it only filters and copies.
	// The copy matters: the event's map is shared with every other
	// subscriber, and the after-response hooks below delete keys from the map
	// they are handed.
	hookIDs := make([]string, 0, 3)
	for _, et := range []hooks.EventType{hooks.AfterCreate, hooks.AfterUpdate, hooks.AfterDelete} {
		id := h.hooks.RegisterDynamic(schemaName, et, func(_ context.Context, ev hooks.Event) error {
			if ev.TenantID != callerTenant {
				return nil
			}
			select {
			case ch <- change{event: string(ev.Type), data: maps.Clone(ev.Data)}:
			default: // drop if buffer full: client is too slow
			}
			return nil
		})
		hookIDs = append(hookIDs, id)
	}
	defer func() {
		for _, id := range hookIDs {
			h.hooks.Unregister(id)
		}
	}()

	_, _ = fmt.Fprintf(w, ": connected to %s stream\n\n", schemaName) // err suppressed: SSE write, client may have disconnected
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case c := <-ch:
			data := mask(c.data)
			h.runAfterResponse(r, schemaName, "list", data)
			payload, err := jsonpool.MarshalJSON(map[string]any{"event": c.event, "schema": schemaName, "data": data})
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload) // err suppressed: SSE write, client may have disconnected
			flusher.Flush()
		}
	}
}
