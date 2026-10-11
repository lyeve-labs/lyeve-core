package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// List returns content entries for a schema with optional filters, pagination, and population.
// GET /api/v1/content/{schema}
func (h *ContentHandler) List(w http.ResponseWriter, r *http.Request) {
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

	limit, err := reqparse.QueryInt(r, "limit", contentListDefaultLimit)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: limit")
		return
	}
	limit = clampLimit(limit, contentListMinLimit, contentListMaxLimit)
	offset, err := reqparse.QueryOffset(r)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: offset")
		return
	}

	// ?filters[author_id]=uuid  ->  map[string]any{"author_id": "uuid"}
	filters, err := parseFilters(r)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: filters")
		return
	}
	loc, locale, ok := h.readLocale(w, r)
	if !ok {
		return
	}

	items, err := h.store.List(r.Context(), schemaName, limit, offset, filters)
	if err != nil {
		storeError(w, r, err, "failed to list content")
		return
	}

	// ?populate=author,tags&depth=2: resolve related records inline
	if !h.populate(w, r, items) {
		return
	}

	for _, item := range items {
		if !h.localize(w, r, loc, locale, item) {
			return
		}
		item.Data = mask(item.Data)
		h.runAfterResponse(r, schemaName, "list", item.Data)
	}

	if setETagAndCheckNotModified(w, r, etagFor(items)) {
		return
	}
	respond(w, http.StatusOK, items)
}

// Get returns a single content entry by ID.
// GET /api/v1/content/{schema}/{id}
func (h *ContentHandler) Get(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "get") {
		return
	}
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
	loc, locale, ok := h.readLocale(w, r)
	if !ok {
		return
	}

	item, err := h.store.GetByID(r.Context(), schemaName, id)
	if err != nil {
		storeError(w, r, err, "failed to get content")
		return
	}

	if !h.populate(w, r, []*domain.Content{item}) {
		return
	}

	if !h.localize(w, r, loc, locale, item) {
		return
	}
	h.runAfterResponse(r, schemaName, "get", item.Data)
	item.Data = mask(item.Data)
	if setETagAndCheckNotModified(w, r, etagFor(item)) {
		return
	}
	respond(w, http.StatusOK, item)
}

// Create inserts a new content entry.
// POST /api/v1/content/{schema}
// Body: {"data": {...}}
func (h *ContentHandler) Create(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "create") {
		return
	}
	if !h.checkPermission(w, r, schemaName, "create") {
		return
	}
	mask, maskOK := h.maskFor(w, r, schemaName)
	if !maskOK {
		return
	}

	var input struct {
		Data map[string]any `json:"data"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &input); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}

	data, ok := h.prepareCreate(w, r, schemaName, input.Data)
	if !ok {
		return
	}
	input.Data = data

	if err := h.hooks.Run(r.Context(), hooks.Event{
		Type: hooks.BeforeCreate, Schema: schemaName, Data: input.Data,
	}); err != nil {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, "blocked by pre-create hook")
		return
	}

	item, err := h.store.Insert(r.Context(), schemaName, input.Data)
	if err != nil {
		storeError(w, r, err, "failed to create content")
		return
	}

	h.publishAfter(r, core.AfterCreate, schemaName, item.ID.String(), nil, item.Data)

	h.runAfterResponse(r, schemaName, "create", item.Data)
	item.Data = mask(item.Data)
	respond(w, http.StatusCreated, item)
}

// Update writes the fields the body carries to an existing content entry and
// leaves every other field as it is stored.
// PUT /api/v1/content/{schema}/{id}
// Body: {"data": {...}}
func (h *ContentHandler) Update(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "update") {
		return
	}
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

	var input struct {
		Data map[string]any `json:"data"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &input); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}

	existing, err := h.store.GetByID(r.Context(), schemaName, id)
	if err != nil {
		storeError(w, r, err, "failed to get content for update")
		return
	}

	if !h.validatePatch(w, r, schemaName, input.Data, existing.Data) {
		return
	}

	if err := h.hooks.Run(r.Context(), hooks.Event{
		Type: hooks.BeforeUpdate, Schema: schemaName, Data: input.Data, OldData: existing.Data,
	}); err != nil {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, "blocked by pre-update hook")
		return
	}

	// Snapshot the pre-update state so the revision history records every
	// write and the restore path has something to restore.
	h.snapshot(r, schemaName, id, existing.Data)

	item, err := h.store.Update(r.Context(), schemaName, id, input.Data, existing.UpdatedAt)
	if err != nil {
		storeError(w, r, err, "failed to update content")
		return
	}

	h.publishAfter(r, core.AfterUpdate, schemaName, id.String(), existing.Data, item.Data)

	h.runAfterResponse(r, schemaName, "update", item.Data)
	item.Data = mask(item.Data)
	respond(w, http.StatusOK, item)
}

// Delete removes a content entry.
// DELETE /api/v1/content/{schema}/{id}
func (h *ContentHandler) Delete(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "delete") {
		return
	}
	if !h.checkPermission(w, r, schemaName, "delete") {
		return
	}

	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}

	existing, err := h.store.GetByID(r.Context(), schemaName, id)
	if err != nil {
		storeError(w, r, err, "failed to get content for delete")
		return
	}

	if err := h.hooks.Run(r.Context(), hooks.Event{
		Type: hooks.BeforeDelete, Schema: schemaName, Data: existing.Data,
	}); err != nil {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, "blocked by pre-delete hook")
		return
	}

	if err := h.store.Delete(r.Context(), schemaName, id); err != nil {
		storeError(w, r, err, "failed to delete content")
		return
	}

	h.publishAfter(r, core.AfterDelete, schemaName, id.String(), existing.Data, nil)

	w.WriteHeader(http.StatusNoContent)
}

// BulkCreate inserts multiple records in a single transaction.
// POST /api/v1/content/{schema}/bulk
// Body: {"items": [{...}, ...]}
func (h *ContentHandler) BulkCreate(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.runBeforeRequest(w, r, schemaName, "create") {
		return
	}
	if !h.checkPermission(w, r, schemaName, "create") {
		return
	}
	mask, maskOK := h.maskFor(w, r, schemaName)
	if !maskOK {
		return
	}

	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(body.Items) == 0 {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "items must not be empty")
		return
	}
	if len(body.Items) > 500 {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "bulk insert limited to 500 items per request")
		return
	}

	for i, item := range body.Items {
		data, ok := h.prepareCreate(w, r, schemaName, item)
		if !ok {
			return
		}
		body.Items[i] = data
	}

	// Run BeforeCreate hook for each item
	for _, item := range body.Items {
		if err := h.hooks.Run(r.Context(), hooks.Event{
			Type: hooks.BeforeCreate, Schema: schemaName, Data: item,
		}); err != nil {
			httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, "blocked by pre-create hook")
			return
		}
	}

	results, err := h.store.BulkInsert(r.Context(), schemaName, body.Items)
	if err != nil {
		httpx.ErrorReq(w, r, httpStatus(err), "bulk insert failed")
		return
	}

	for _, item := range results {
		h.publishAfter(r, core.AfterCreate, schemaName, item.ID.String(), nil, item.Data)
		item.Data = mask(item.Data)
	}

	respond(w, http.StatusCreated, results)
}

// ListCursor returns records using cursor-based (keyset) pagination.
// GET /api/v1/content/{schema}/cursor
func (h *ContentHandler) ListCursor(w http.ResponseWriter, r *http.Request) {
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

	cursor := r.URL.Query().Get("cursor")
	limit, err := reqparse.QueryInt(r, "limit", 20)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: limit")
		return
	}
	loc, locale, ok := h.readLocale(w, r)
	if !ok {
		return
	}

	items, err := h.store.ListCursor(r.Context(), schemaName, cursor, limit)
	if err != nil {
		storeError(w, r, err, "failed to list content")
		return
	}

	// ?populate=author,tags&depth=2: resolve related records inline
	if !h.populate(w, r, items) {
		return
	}

	var nextCursor string
	if len(items) == limit && len(items) > 0 {
		nextCursor = items[len(items)-1].ID.String()
	}

	for _, item := range items {
		if !h.localize(w, r, loc, locale, item) {
			return
		}
		item.Data = mask(item.Data)
		h.runAfterResponse(r, schemaName, "list", item.Data)
	}

	respond(w, http.StatusOK, map[string]any{
		"data":        items,
		"next_cursor": nextCursor,
	})
}

// parsePopulateFromQuery builds a PopulateConfig from ?populate and ?depth
// query parameters. Supports the enhanced population syntax:
//
//	?populate=author,tags - comma-separated field names
//	?populate=author.avatar,category - dot-notation for nested population
//	?populate=* - wildcard: all relation fields at root
//	?populate=author.* - wildcard: all relations within author
//	?depth=2 - auto-populate all relations up to N levels
//	?populate=author.avatar&depth=1 - combine explicit paths + auto-depth
func parsePopulateFromQuery(r *http.Request) domain.PopulateConfig {
	cfg := domain.PopulateConfig{}

	// Parse ?populate=  (comma-separated, dot-notation for nesting)
	if pop := r.URL.Query().Get("populate"); pop != "" {
		cfg.Paths = strings.Split(pop, ",")
		for i, p := range cfg.Paths {
			cfg.Paths[i] = strings.TrimSpace(p)
		}
	}

	// Parse ?depth=  (auto-populate up to N levels, Payload CMS style)
	if d := r.URL.Query().Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 {
			cfg.MaxDepth = n
		}
	}

	return cfg
}

// parseFilters reads ?filters[col]=val query params into a map.
// Column names are not validated here: ContentStore.List checks them against a
// predefined allowlist (system columns + resolved schema fields) and returns a
// domain.ErrBadRequest error for unknown keys, which the List handler maps to HTTP 400.
//
// Returns an error when a malformed filter key is detected: specifically, any
// key that starts with "filters[" but lacks a closing "]" (e.g. filters[1=1--
// where the '=' inside the key was parsed as the HTTP KV separator). This
// prevents SQL injection payloads from silently bypassing the allowlist.
func parseFilters(r *http.Request) (map[string]any, error) {
	out := map[string]any{}
	q := r.URL.Query()
	prefix := "filters["
	for key, vals := range q {
		// Detect malformed filter keys: starts with filters[ but no closing ].
		if strings.HasPrefix(key, prefix) {
			if !strings.HasSuffix(key, "]") {
				return nil, fmt.Errorf("malformed filter key: %q (missing closing bracket)", key)
			}
			col := key[8 : len(key)-1]
			if col != "" && len(vals) > 0 {
				if _, exists := out[col]; !exists {
					out[col] = vals[0]
				}
			}
		}
	}
	return out, nil
}

// storeError answers a failed store call and, when the answer is a 5xx, writes
// the driver error to the log. The message on the wire is deliberately static,
// so without this the whole class of engine-side content failures would reach
// the caller as "failed to create content" and the operator as nothing at all:
// a 503 in the access log with no line anywhere saying why.
func storeError(w http.ResponseWriter, r *http.Request, err error, msg string) {
	// An install with no schema engine says so, on every route, rather than
	// letting the absence read as a missing content type or an empty page.
	if errors.Is(err, core.ErrNoSchemaEngine) {
		httpx.ErrorReq(w, r, http.StatusServiceUnavailable,
			"no schema engine is installed, so this install cannot serve content")
		return
	}
	status := httpStatus(err)
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "content: "+msg, "schema", chi.URLParam(r, "schema"), "status", status, "err", err)
	}
	httpx.ErrorReq(w, r, status, msg)
}

func httpStatus(err error) int {
	if errors.Is(err, core.ErrNoSchemaEngine) {
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, core.ErrRecordRevisionNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, domain.ErrConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, domain.ErrForbidden) {
		return http.StatusForbidden
	}
	if errors.Is(err, domain.ErrUnauth) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, domain.ErrBadRequest) {
		return http.StatusBadRequest
	}
	if errors.Is(err, domain.ErrValidation) {
		return http.StatusUnprocessableEntity
	}
	if plugin.IsTableNotExistError(err) {
		// relation/table does not exist: schema was deleted concurrently
		return http.StatusNotFound
	}
	return httpx.StoreStatusFor(err)
}

// etagFor computes a weak ETag from the JSON representation of v.
// Returns a string of the form `W/"<hex>"`.
func etagFor(v any) string {
	b, err := jsonpool.MarshalJSON(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf(`W/"%x"`, sum[:8])
}

// setETagAndCheckNotModified writes the ETag header and returns true (304) when
// the client's If-None-Match header matches. Must be called before writing body.
func setETagAndCheckNotModified(w http.ResponseWriter, r *http.Request, etag string) bool {
	if etag == "" {
		return false
	}
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// contentListLimits bound a content list page: minimum 1, matching the
// OpenAPI document. The generator reads these constants, so the contract and
// the clamp cannot drift apart.
const (
	contentListMinLimit     = 1
	contentListMaxLimit     = 200
	contentListDefaultLimit = 25
)

// clampLimit enforces safe bounds on a parsed limit query parameter. A limit
// below min is clamped to min, and one above max is clamped to max.
//
// The store already skips list-cache writes above 200. The handler-side
// clamp keeps the whole hot path (DB scan, JSON marshal, populate, cache)
// inside a known-safe window, and the cursor store applies its own clamp
// (at most 1000).
func clampLimit(limit, min, max int) int {
	if limit < min {
		return min
	}
	if limit > max {
		return max
	}
	return limit
}
