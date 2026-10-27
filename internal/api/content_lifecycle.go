package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
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
	// A revision is a snapshot of a state the row passed through, drafts
	// included. A caller who cannot read drafts gets the history of a row
	// they can read, and only its published states.
	drafts := h.draftResponse(w, r, schemaName)
	if !drafts {
		current, err := h.store.GetByID(r.Context(), schemaName, id)
		if err != nil {
			storeError(w, r, err, "failed to list revisions")
			return
		}
		if !isPublished(current.Data) {
			httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
			return
		}
	}
	revisions, err := store.ListRecordRevisions(r.Context(), schemaName, id)
	if err != nil {
		revisionError(w, r, err, "failed to list revisions")
		return
	}
	served := make([]core.RecordRevision, 0, len(revisions))
	for _, rev := range revisions {
		if !drafts && !isPublished(rev.Data) {
			continue
		}
		rev.Data = mask(rev.Data)
		served = append(served, rev)
	}
	respond(w, http.StatusOK, served)
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
	// The permission check covered the path's type and record only, so a
	// revision of any other record is as absent as an unknown id.
	if rev.SchemaName != schemaName || rev.RecordID != id {
		httpx.ErrorReq(w, r, http.StatusNotFound, "failed to get revision")
		return
	}
	// The restore needs the record's current version to write against, so a
	// read failure here is the caller's answer.
	existing, err := h.store.GetByID(r.Context(), schemaName, id)
	if err != nil {
		httpx.ErrorReq(w, r, httpStatus(err), "failed to get content for restore")
		return
	}
	// A revision can predate the content type's current rules, so it is
	// checked as an update of every field it holds before anything is written.
	restored := withoutStatus(rev.Data)
	if !h.validatePatch(w, r, schemaName, restored, existing.Data) {
		return
	}
	h.snapshot(r, schemaName, id, existing.Data)
	item, err := h.store.Update(r.Context(), schemaName, id, restored, existing.UpdatedAt)
	if err != nil {
		if !relationRefused(w, r, err) {
			httpx.ErrorReq(w, r, httpStatus(err), "failed to restore revision")
		}
		return
	}
	item.Data = mask(item.Data)
	respond(w, http.StatusOK, item)
}

// contentStreamHeartbeat is how often a change stream with nothing to send
// writes a comment line. It sits inside every timeout between the engine and
// a browser: the API listener's 15 second write timeout and the 60 second
// idle timeout most proxies and load balancers apply to a response that has
// gone quiet.
const contentStreamHeartbeat = 10 * time.Second

// contentStreamEvents are the writes a change stream sends, one event each.
// The API document lists the same names, so a client generated from it knows
// every event the stream can carry.
var contentStreamEvents = []hooks.EventType{hooks.AfterCreate, hooks.AfterUpdate, hooks.AfterDelete}

// The comment lines a change stream writes: the first line of every stream,
// a format taking the schema name, and the heartbeat. A comment carries no
// event, so an EventSource never reports one. The API document quotes both.
const (
	contentStreamConnectedComment = ": connected to %s stream"
	contentStreamHeartbeatComment = ": heartbeat"
	// contentStreamErrorEvent names the one event the stream sends with an
	// event field: the last one, saying why the stream stopped.
	contentStreamErrorEvent = "error"
)

// streamHeartbeatInterval is the handler's heartbeat, or the default when
// none was set.
func (h *ContentHandler) streamHeartbeatInterval() time.Duration {
	if h.streamHeartbeat > 0 {
		return h.streamHeartbeat
	}
	return contentStreamHeartbeat
}

// warnNoStreamDeadline logs that change streams cannot hold their own write
// deadline. Every stream through the same chain fails the same way, so it is
// said once.
func (h *ContentHandler) warnNoStreamDeadline(err error) {
	warn := func() {
		slog.Warn("content: change streams cannot extend their write deadline, so each ends at the server's write timeout. A response writer in the middleware chain does not implement Unwrap.",
			"err", err)
	}
	if h.streamDeadlineWarn == nil {
		warn()
		return
	}
	h.streamDeadlineWarn.Do(warn)
}

// Stream sends a Server-Sent Events stream of content change events for a schema.
// GET /api/v1/content/{schema}/stream
//
// The hooks it registers fire for every write to the schema, in every tenant,
// because a generated table is shared and keyed by its tenant column. So the
// stream keeps only the events of the caller's own tenant, and each payload
// goes through the field mask and the after-response hooks a read applies,
// so the stream serves no field that GET would withhold from this caller.
//
// The read grant, the draft grant, the mask and whether the schema has draft
// and publish are asked again for every event, not held from the open. A
// stream lives for hours, and an operator who revokes a read, takes away
// draft access, hides a field or switches draft and publish on expects the
// next event to obey it, not the next reconnect. A refused read ends the stream
// after an error event, and so does a rule store that cannot answer, because
// an event it cannot judge is not sent. A change that may be to a draft is
// skipped while the caller may not read drafts, and the stream stays open.
//
// A credential holds at most contentStreamsPerCredential streams open and a
// tenant at most contentStreamsPerTenant, because each write costs every open
// stream those checks. The next stream past either cap is refused with a 429
// before anything is sent, and a stream gives its slot back however it ends.
//
// The credential is held to the same terms for as long as the stream lives.
// A token ends the stream when it expires, and each event and each heartbeat
// first asks whether the credential still admits the caller, so a sign-out, a
// password a super admin sets, an erasure or a revoked key ends it within a
// heartbeat. An idle stream left open by a sign-out then frees its slot for
// the account's next session.
//
// The server's write deadline is set once, when it reads the request, and the
// first write after it fails and ends the stream. So every write moves the
// deadline two heartbeats ahead first, and a comment line every heartbeat
// keeps an idle stream inside it.
//
// None of those checks reads on the request's tenant connection. Each reads
// the engine's own tables, which resolve the same on any connection, and the
// first read on the request's connection would take it for as long as the
// stream stays open. So an idle stream holds no connection between changes.
func (h *ContentHandler) Stream(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(db.WithoutTakingTenantConn(r.Context()))
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "list") {
		return
	}
	if !h.checkPermission(w, r, schemaName, "read") {
		return
	}
	if _, ok := h.maskFor(w, r, schemaName); !ok {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	callerTenant := core.TenantIDFromCtx(r.Context())
	credential := streamCredentialKey(r)
	if !h.streamSlots.take(callerTenant, credential) {
		w.Header().Set("Retry-After", contentStreamRetryAfter)
		httpx.ErrorReq(w, r, http.StatusTooManyRequests, "too many open change streams")
		return
	}
	defer h.streamSlots.release(callerTenant, credential)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")

	type change struct {
		event string
		data  map[string]any
	}
	ch := make(chan change, 16)

	// The hook runs on the writer's goroutine, so it only filters and copies.
	// The copy matters: the event's map is shared with every other
	// subscriber, and the after-response hooks below delete keys from the map
	// they are handed.
	hookIDs := make([]string, 0, len(contentStreamEvents))
	for _, et := range contentStreamEvents {
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

	heartbeat := h.streamHeartbeatInterval()
	deadline := httpx.NewStreamDeadline(w, 2*heartbeat, h.warnNoStreamDeadline)
	if !deadline.Hold() {
		return
	}
	// end sends the error event that says why the stream stops. The caller
	// returns after it.
	end := func(err error) {
		if !deadline.Hold() {
			return
		}
		_, _ = fmt.Fprintf(w, "event: "+contentStreamErrorEvent+"\ndata: {\"error\":%q}\n\n", streamEndReason(err)) // err suppressed: SSE write, client may have disconnected
		flusher.Flush()
	}
	var expired <-chan time.Time
	if c := claimsFromCtx(r); c != nil && c.ExpiresAt != nil {
		expiry := time.NewTimer(time.Until(c.ExpiresAt.Time))
		defer expiry.Stop()
		expired = expiry.C
	}
	_, _ = fmt.Fprintf(w, contentStreamConnectedComment+"\n\n", schemaName) // err suppressed: SSE write, client may have disconnected
	flusher.Flush()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if h.streamCredential != nil {
				if err := h.streamCredential(r); err != nil {
					end(err)
					return
				}
			}
			if !deadline.Hold() {
				return
			}
			_, _ = fmt.Fprint(w, contentStreamHeartbeatComment+"\n\n") // err suppressed: SSE write, client may have disconnected
			flusher.Flush()
		case <-expired:
			end(errCredentialEnded)
			return
		case c := <-ch:
			mask, send, err := h.streamAccess(r, schemaName, c.data)
			if err != nil {
				end(err)
				return
			}
			if !send {
				continue
			}
			data := mask(c.data)
			h.runAfterResponse(r, schemaName, "list", data)
			payload, err := jsonpool.MarshalJSON(map[string]any{"event": c.event, "schema": schemaName, "data": data})
			if err != nil {
				continue
			}
			if !deadline.Hold() {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload) // err suppressed: SSE write, client may have disconnected
			flusher.Flush()
		}
	}
}

// streamAccess asks whether the caller may still read schema and resolves
// the mask that applies now. It is the open's checks without a response,
// because by the time an event arrives the headers have been sent. send is
// false for a change that may be to a draft when the caller may not read
// drafts now, which skips that change without ending the stream.
func (h *ContentHandler) streamAccess(r *http.Request, schema string, data map[string]any) (mask func(map[string]any) map[string]any, send bool, err error) {
	if h.streamCredential != nil {
		if err := h.streamCredential(r); err != nil {
			return nil, false, err
		}
	}
	if err := h.authorize(r, schema, "read"); err != nil {
		return nil, false, err
	}
	if !core.ContentEventPublished(h.streamDraftPublish(r, schema), data) && !h.canReadDrafts(r, schema) {
		return nil, false, nil
	}
	mask, err = h.fieldMask(r, schema)
	if err != nil {
		return nil, false, err
	}
	return mask, true, nil
}

// streamDraftPublish reports whether schema has draft and publish now. It is
// asked for every event, because an operator can switch the mode on while a
// stream is open and every change after that may be a draft. On a schema with
// draft and publish a change without a status counts as one: a writer that
// sends only the columns it changed may be writing a draft. A schema the
// source cannot read is treated as having draft and publish, so a failed
// lookup withholds a change rather than sending it. A handler built with no
// source at all has no schema that could have one.
func (h *ContentHandler) streamDraftPublish(r *http.Request, schema string) bool {
	if h.schemas == nil {
		return false
	}
	sc, err := h.schemas.GetByName(r.Context(), schema)
	if err != nil || sc == nil {
		return true
	}
	return sc.WithDraftPublish
}

// streamEndReason is the message the error event carries, in the words the
// content routes answer with, and never the rule store's own text.
func streamEndReason(err error) string {
	switch {
	case errors.Is(err, errCredentialEnded):
		return "credential expired or revoked"
	case errors.Is(err, errNoCaller):
		return "authentication required"
	case errors.Is(err, errSchemaNotAllowed):
		return "schema not allowed for this credential"
	case errors.Is(err, errScopeDenied):
		return "insufficient scope"
	case errors.Is(err, errPermissionDenied):
		return "permission denied"
	default:
		return "access could not be checked"
	}
}
