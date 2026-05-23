package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	internalschema "github.com/lyeve-labs/lyeve-core/internal/schema"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// ContentHandler serves the content CRUD API for schema-defined resources.
type ContentHandler struct {
	store     *db.ContentStore
	schemas   core.SchemaSource
	hooks     *hooks.Registry
	perms     core.PermissionCheckerProvider   // nil = the kernel's role-only checker
	localizer core.ContentLocalizerProvider    // nil = reads ignore any locale
	revisions core.RecordRevisionStoreProvider // nil = no plugin keeps revisions
}

// revisionStore resolves the store that keeps revision history, or answers
// 503 and reports false. A build with no plugin keeping revisions has no
// history, and saying so is the only honest answer: an empty list reads as a
// record that was never edited, and a 404 reads as a record that is not
// there. Both are wrong in the way that looks like data loss.
func (h *ContentHandler) revisionStore(w http.ResponseWriter, r *http.Request) (core.RecordRevisionStore, bool) {
	if h.revisions != nil {
		if s := h.revisions.RecordRevisionStore(); s != nil {
			return s, true
		}
	}
	httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "revision history is unavailable: no plugin keeps it")
	return nil, false
}

// snapshot records the state of a record before the write that replaces it.
// A failure never fails the write: the update the caller asked for has
// already been validated and the history is the secondary record. With no
// plugin keeping revisions there is nothing to record and nothing to warn
// about, which is the ordinary state of a build without one.
func (h *ContentHandler) snapshot(r *http.Request, schemaName string, id uuid.UUID, data map[string]any) {
	if h.revisions == nil {
		return
	}
	store := h.revisions.RecordRevisionStore()
	if store == nil {
		return
	}
	if err := store.SaveRecordRevision(r.Context(), schemaName, id, data, actorID(r)); err != nil {
		slog.WarnContext(r.Context(), "content: pre-update snapshot failed", "schema", schemaName, "id", id, "err", err)
	}
}

// readLocale resolves the locale a read is served in and the localizer that
// serves it. Both are empty when no plugin has registered a localizer: the
// locale parameter is then not even parsed, so a read ignores any locale the
// request carries. With a localizer the query parameter wins over
// Accept-Language, and a parameter that is not a language tag is a 400.
// Returns false after answering.
//
// The response then depends on Accept-Language whenever a localizer is
// registered, whether or not this request named a locale, and the content
// routes are served with a public cache policy: Vary says so, or a shared
// cache would hand one caller's language to the next.
func (h *ContentHandler) readLocale(w http.ResponseWriter, r *http.Request) (core.ContentLocalizer, string, bool) {
	if h.localizer == nil {
		return nil, "", true
	}
	loc := h.localizer.ContentLocalizer()
	if loc == nil {
		return nil, "", true
	}
	w.Header().Add("Vary", "Accept-Language")
	locale, err := core.RequestedLocale(r)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: locale")
		return nil, "", false
	}
	return loc, locale, true
}

// localize serves item's fields in locale through loc, when both are set,
// and records the locale they came from on the item. A localizer failure
// is a store failure: the read is answered 503 rather than served in the
// wrong language, and false is returned after answering. Runs before the
// field mask, so a masked field a translation carries stays masked.
func (h *ContentHandler) localize(w http.ResponseWriter, r *http.Request, loc core.ContentLocalizer, locale string, item *domain.Content) bool {
	if loc == nil || locale == "" || item == nil {
		return true
	}
	data, resolved, err := loc.Localize(r.Context(), item.SchemaName, item.ID.String(), locale, item.Data)
	if err != nil {
		slog.WarnContext(r.Context(), "content: localizer failed", "schema", item.SchemaName, "locale", locale, "err", err)
		httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "localization unavailable")
		return false
	}
	item.Data = data
	item.ResolvedLocale = resolved
	return true
}

// contentMethodWrites says whether a content handler's method string is a
// write. It is the only place the two sets are named, so a handler added later
// that passes an unrecognized method is treated as a write and gated by the
// stricter half rather than slipping past.
func contentMethodWrites(method string) bool {
	switch method {
	case "list", "get":
		return false
	default:
		return true
	}
}

// checkTransport refuses a request the schema does not serve over REST.
//
// A schema that is off here is absent rather than forbidden, so it answers 404:
// the content API genuinely has no such resource, and a 403 would confirm the
// schema exists to a caller who is not meant to see it at all. A schema served
// read-only answers 405 to a write, which is the accurate statement that the
// resource exists and the method does not apply to it.
//
// A schema that cannot be loaded is served, and so is every schema when no
// schema store is wired. The gate narrows a surface that is open by default,
// so neither a failed read nor a handler assembled without a store may close a
// schema whose definition never restricted it.
func (h *ContentHandler) checkTransport(w http.ResponseWriter, r *http.Request, schema, method string) bool {
	if h.schemas == nil {
		return true
	}
	s, err := h.schemas.GetByName(r.Context(), schema)
	if err != nil || s == nil {
		return true
	}
	if !s.Transports.Serves(core.TransportREST) {
		httpx.ErrorReq(w, r, http.StatusNotFound, "schema not found")
		return false
	}
	if contentMethodWrites(method) {
		if !s.Transports.CanWrite(core.TransportREST) {
			httpx.ErrorReq(w, r, http.StatusMethodNotAllowed, "schema is read-only over this transport")
			return false
		}
		return true
	}
	if !s.Transports.CanRead(core.TransportREST) {
		httpx.ErrorReq(w, r, http.StatusMethodNotAllowed, "schema is write-only over this transport")
		return false
	}
	return true
}

// runBeforeRequest gates the transport, then fires the BeforeRequest hook.
// Returns false if the handler should abort.
func (h *ContentHandler) runBeforeRequest(w http.ResponseWriter, r *http.Request, schema, method string) bool {
	if !h.checkTransport(w, r, schema, method) {
		return false
	}
	if err := h.hooks.Run(r.Context(), hooks.Event{
		Type:   hooks.BeforeRequest,
		Schema: schema,
		Method: method,
		Data:   map[string]any{"method": method},
	}); err != nil {
		httpx.ErrorReq(w, r, http.StatusForbidden, "blocked by pre-request hook")
		return false
	}
	return true
}

// publishAfter fires the after-write lifecycle event through the one builder
// every content writer shares, so a plugin subscribed to the event sees the
// same shape whichever transport carried the write. The write has committed,
// so a subscriber's failure is logged and the response still answers.
func (h *ContentHandler) publishAfter(r *http.Request, action core.EventType, schema, id string, before, after map[string]any) {
	if err := core.PublishContentEvent(r.Context(), h.hooks, core.SourceEngine, action, schema, id, before, after); err != nil {
		slog.WarnContext(r.Context(), "content: after-write hook failed", "schema", schema, "event", action, "err", err)
	}
}

// runAfterResponse fires the AfterResponse hook on a data map in-place.
// Errors are logged and ignored (non-blocking).
func (h *ContentHandler) runAfterResponse(r *http.Request, schema, method string, data map[string]any) {
	_ = h.hooks.Run(r.Context(), hooks.Event{ // err suppressed: after-action hooks are best-effort
		Type:   hooks.AfterResponse,
		Schema: schema,
		Method: method,
		Data:   data,
	})
}

// checker is the authorization this handler enforces. Never nil: with no rule
// engine supplied it is the kernel's role-only checker, which allows
// super_admin everything and refuses every other role on a schema, so an
// install without a rule engine is closed, not open.
//
// Read per request, never held: the plugin that owns the rules registers its
// checker when it starts and clears it when it stops, so a request served
// after it stops falls back rather than calling into a stopped plugin.
func (h *ContentHandler) checker() core.PermissionChecker {
	if h.perms != nil {
		if c := h.perms.PermissionChecker(); c != nil {
			return c
		}
	}
	return core.RoleOnlyPermissionChecker{}
}

// checkPermission reports whether the caller's roles allow action on schema,
// answering 403 when they do not and 503 when the rules cannot be read.
//
// A session and an API key are judged alike: the key's roles go through the
// same rules a user's do. A credential limited to a list of schemas is
// refused any schema outside it before the rules are asked.
//
// No caller at all is refused. requireAuth runs first on every route that
// reaches here, so a missing caller means the handler was mounted without it,
// and an authorization gate that waves through what it cannot identify is
// open to everyone.
//
// super_admin is exempt, as it is at every other authorization gate. The rule
// store ships empty and only a super_admin can write to it, so without the
// exemption a fresh install answers 403 to every content request whatever the
// caller's role, and the one role able to grant access is the one refused.
func (h *ContentHandler) checkPermission(w http.ResponseWriter, r *http.Request, schema, action string) bool {
	switch err := h.authorize(r, schema, action); {
	case err == nil:
		return true
	case errors.Is(err, errNoCaller):
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
	case errors.Is(err, errSchemaNotAllowed):
		httpx.ErrorReq(w, r, http.StatusForbidden, "schema not allowed for this credential")
	case errors.Is(err, errScopeDenied):
		httpx.ErrorReq(w, r, http.StatusForbidden, "insufficient scope")
	case errors.Is(err, errPermissionDenied):
		httpx.ErrorReq(w, r, http.StatusForbidden, "permission denied")
	default:
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "database error")
	}
	return false
}

// The refusals authorize reports. Anything else it returns is the rule
// store failing.
var (
	errNoCaller         = errors.New("no caller")
	errSchemaNotAllowed = errors.New("schema not allowed for this credential")
	errScopeDenied      = errors.New("insufficient scope")
	errPermissionDenied = errors.New("permission denied")
)

// authorize is checkPermission without the response, for a caller that has
// to decide what to do with a refusal itself, such as a populated relation
// that is left out rather than failing the read it belongs to.
func (h *ContentHandler) authorize(r *http.Request, schema, action string) error {
	claims := requestClaims(r)
	if claims == nil {
		return errNoCaller
	}
	if !claims.AllowsSchema(schema) {
		return errSchemaNotAllowed
	}
	// The scope gate in front of the content routes reads the schema the
	// path names. A relation and a populated field reach a second schema the
	// path never names, so a key held to content.posts would read authors
	// through posts. Each schema a request touches is held to the key's
	// scope here, on the same terms the gate uses: a key with an admin role
	// answers to its role instead.
	if claims.IsAPIKey && !claims.HasRole("admin") && !claims.HasRole("super_admin") &&
		!core.ScopeGrants(claims.Scopes, "content."+schema, action) {
		return errScopeDenied
	}
	allowed, err := h.checker().Allowed(r.Context(), claims.Roles, schema, action)
	if err != nil {
		return fmt.Errorf("permission check %s: %w", schema, err)
	}
	if !allowed {
		return errPermissionDenied
	}
	return nil
}

// maskFor resolves this request's field mask once and returns the function
// that applies it to an entry. Returns false after answering when the mask
// cannot be resolved, because a read served with fields the caller may not
// see is worse than a read that fails.
//
// One resolution per request, not one per entry. A list of a hundred entries
// asked for the same mask a hundred times, once per role each time.
//
// A checker with no field rules does not implement core.FieldMasker, and the
// kernel's role-only checker is one of those: a mask only ever comes from a
// rule, so an install with no rule store hides nothing.
func (h *ContentHandler) maskFor(w http.ResponseWriter, r *http.Request, schema string) (func(map[string]any) map[string]any, bool) {
	mask, err := h.fieldMask(r, schema)
	switch {
	case err == nil:
		return mask, true
	case errors.Is(err, errNoCaller):
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
	default:
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "database error")
	}
	return nil, false
}

// fieldMask is maskFor without the response.
func (h *ContentHandler) fieldMask(r *http.Request, schema string) (func(map[string]any) map[string]any, error) {
	keep := func(data map[string]any) map[string]any { return data }
	masker, ok := h.checker().(core.FieldMasker)
	if !ok {
		return keep, nil
	}
	claims := requestClaims(r)
	if claims == nil {
		return nil, errNoCaller
	}
	fields, err := masker.FieldMask(r.Context(), claims.Roles, schema)
	if err != nil {
		return nil, fmt.Errorf("field mask %s: %w", schema, err)
	}
	if len(fields) == 0 {
		return keep, nil
	}
	hidden := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		hidden[f] = struct{}{}
	}
	return func(data map[string]any) map[string]any {
		if len(data) == 0 {
			return data
		}
		out := make(map[string]any, len(data))
		for k, v := range data {
			if _, off := hidden[k]; !off {
				out[k] = v
			}
		}
		return out
	}, nil
}

// populate resolves ?populate= and ?depth= on items, putting every relation
// the walk reaches through the same checks a direct read of its target gets.
// A relation into a schema the caller may not read, by its credential's list
// or by the rules, is left out of the entry, and one it may read comes back
// with the target's field mask applied.
//
// Population stays best-effort for a relation the store cannot read. A rule
// store that cannot answer is different: the read answers 503 rather than
// guess, and populate reports false after writing it.
func (h *ContentHandler) populate(w http.ResponseWriter, r *http.Request, items []*domain.Content) bool {
	cfg := parsePopulateFromQuery(r)
	if cfg.IsEmpty() || len(items) == 0 {
		return true
	}
	var ruleErr error
	type decision struct {
		mask func(map[string]any) map[string]any
		ok   bool
	}
	decided := map[string]decision{}
	gate := func(_ context.Context, target string) (func(map[string]any) map[string]any, bool) {
		if d, seen := decided[target]; seen {
			return d.mask, d.ok
		}
		d := decision{}
		switch err := h.authorize(r, target, "read"); {
		case err == nil:
			mask, mErr := h.fieldMask(r, target)
			if mErr != nil {
				ruleErr = mErr
			} else {
				d = decision{mask: mask, ok: true}
			}
		case errors.Is(err, errSchemaNotAllowed), errors.Is(err, errScopeDenied), errors.Is(err, errPermissionDenied), errors.Is(err, errNoCaller):
		default:
			ruleErr = err
		}
		decided[target] = d
		return d.mask, d.ok
	}
	_ = h.store.PopulateGated(r.Context(), items, cfg, gate) // err suppressed: a relation the store cannot read is served empty
	if ruleErr != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(ruleErr), "database error")
		return false
	}
	return true
}

// validatePatch validates the body of a partial update: only the fields it
// carries, read against the stored entry for cross-field rules. Returns true
// if the patch is valid, and false if the refusal has been written to w.
func (h *ContentHandler) validatePatch(w http.ResponseWriter, r *http.Request, schemaName string, patch, current map[string]any) bool {
	s, ok := h.schemaForValidation(w, r, schemaName)
	if !ok {
		return false
	}
	errs := internalschema.ValidatePatch(s, patch, current)
	if len(errs) == 0 {
		return true
	}
	internalschema.ValidationErrors(errs).Respond(w, r, http.StatusUnprocessableEntity)
	return false
}

// prepareCreate fills the declared defaults into data and validates the
// result, in that order: a required field with a default is satisfied by
// omission on a create, which validating first would refuse. The update path does
// not fill defaults, since an omitted field there is one to leave alone.
// Returns the data to insert, or false when the refusal has been written.
func (h *ContentHandler) prepareCreate(w http.ResponseWriter, r *http.Request, schemaName string, data map[string]any) (map[string]any, bool) {
	s, ok := h.schemaForValidation(w, r, schemaName)
	if !ok {
		return nil, false
	}
	data = internalschema.ApplyDefaults(s, data)
	return data, h.validateAgainst(w, r, s, data)
}

func (h *ContentHandler) schemaForValidation(w http.ResponseWriter, r *http.Request, schemaName string) (*domain.Schema, bool) {
	s, err := h.schemas.GetByName(r.Context(), schemaName)
	if err != nil {
		storeError(w, r, err, "failed to load schema for validation")
		return nil, false
	}
	return s, true
}

func (h *ContentHandler) validateAgainst(w http.ResponseWriter, r *http.Request, s *domain.Schema, data map[string]any) bool {
	errs := internalschema.ValidateContent(s, data)
	if len(errs) == 0 {
		return true
	}
	internalschema.ValidationErrors(errs).Respond(w, r, http.StatusUnprocessableEntity)
	return false
}
