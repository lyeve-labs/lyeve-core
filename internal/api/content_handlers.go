package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
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
	hidden, maskOK := h.hiddenFor(w, r, schemaName)
	if !maskOK {
		return
	}
	mask := hidden.Apply
	fields, ok := h.readFields(w, r, schemaName, hidden)
	if !ok {
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
	conds, ok := takeConditions(r.URL.Query(), filters)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: filters")
		return
	}
	ranges := takeRanges(filters)
	if conds, ok = takePaths(filters, ranges, conds); !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: filters")
		return
	}
	sort, ok := parseListSort(r)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: sort")
		return
	}
	if !h.refuseHiddenFilters(w, r, schemaName, queriedColumns(filters, ranges, conds, sort)) {
		return
	}
	loc, locale, ok := h.readLocale(w, r)
	if !ok {
		return
	}

	if !h.checkStatusFilter(w, r, schemaName, filters) {
		return
	}
	h.draftResponse(w, r, schemaName)

	items, err := h.store.ListPage(r.Context(), schemaName, db.ListQuery{
		Limit: limit, Offset: offset, Filters: filters, Ranges: ranges, Conditions: conds, Sort: sort,
		PathGate: h.pathGate(w, r),
	})
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
		item.Data = fields.apply(item.Data)
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
	hidden, maskOK := h.hiddenFor(w, r, schemaName)
	if !maskOK {
		return
	}
	mask := hidden.Apply
	fields, ok := h.readFields(w, r, schemaName, hidden)
	if !ok {
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
	// The list leaves unpublished rows out, and a read by id has to agree,
	// or an id learned from a webhook, a log or a guess reads a draft. The
	// answer is the one a missing row gets, so the read does not confirm
	// that the draft exists.
	if drafts := h.draftResponse(w, r, schemaName); !drafts && !isPublished(item.Data) {
		httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
		return
	}

	if !h.populate(w, r, []*domain.Content{item}) {
		return
	}

	if !h.localize(w, r, loc, locale, item) {
		return
	}
	h.runAfterResponse(r, schemaName, "get", item.Data)
	item.Data = fields.apply(mask(item.Data))
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
	sc, scErr := h.writeSchema(r, schemaName)
	if err := decodeContentBody(r.Body, sc, &input); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}
	if scErr != nil {
		storeError(w, r, scErr, "failed to load content type for validation")
		return
	}
	if err := coerceNumbers(sc, input.Data); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}

	data, ok := h.prepareCreate(w, r, sc, input.Data)
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
	sc, scErr := h.writeSchema(r, schemaName)
	if err := decodeContentBody(r.Body, sc, &input); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}
	if scErr != nil {
		storeError(w, r, scErr, "failed to load content type for validation")
		return
	}
	input.Data = withoutStatus(input.Data)
	if err := coerceNumbers(sc, input.Data); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}

	existing, err := h.store.GetByID(r.Context(), schemaName, id)
	if err != nil {
		storeError(w, r, err, "failed to get content for update")
		return
	}

	if !h.validatePatch(w, r, sc, input.Data, existing.Data) {
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
	sc, scErr := h.writeSchema(r, schemaName)
	if err := decodeContentBody(r.Body, sc, &body); err != nil {
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
	if scErr != nil {
		storeError(w, r, scErr, "failed to load content type for validation")
		return
	}

	for _, item := range body.Items {
		if err := coerceNumbers(sc, item); err != nil {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	for i, item := range body.Items {
		data, ok := h.prepareCreate(w, r, sc, item)
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
		if !relationRefused(w, r, err) {
			httpx.ErrorReq(w, r, storeFailureStatus(r, err), "bulk insert failed")
		}
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
	hidden, maskOK := h.hiddenFor(w, r, schemaName)
	if !maskOK {
		return
	}
	mask := hidden.Apply
	fields, ok := h.readFields(w, r, schemaName, hidden)
	if !ok {
		return
	}

	cursor := r.URL.Query().Get("cursor")
	limit, err := reqparse.QueryInt(r, "limit", 20)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: limit")
		return
	}
	sort, ok := parseListSort(r)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: sort")
		return
	}
	if !h.refuseHiddenFilters(w, r, schemaName, queriedColumns(nil, nil, nil, sort)) {
		return
	}
	loc, locale, ok := h.readLocale(w, r)
	if !ok {
		return
	}

	items, err := h.store.ListCursorSorted(r.Context(), schemaName, cursor, limit, h.draftResponse(w, r, schemaName), sort)
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
		item.Data = fields.apply(item.Data)
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
//
// ok is false for a depth that is not an integer from 0 to db.MaxPopulateDepth.
func parsePopulateFromQuery(r *http.Request) (cfg domain.PopulateConfig, ok bool) {
	// Parse ?populate=  (comma-separated, dot-notation for nesting)
	if pop := r.URL.Query().Get("populate"); pop != "" {
		cfg.Paths = strings.Split(pop, ",")
		for i, p := range cfg.Paths {
			cfg.Paths[i] = strings.TrimSpace(p)
		}
	}

	// Parse ?depth=  (auto-populate up to N levels, Payload CMS style)
	if d := r.URL.Query().Get("depth"); d != "" {
		n, ok := plainInt(d, 0, db.MaxPopulateDepth)
		if !ok {
			return cfg, false
		}
		cfg.MaxDepth = n
	}

	return cfg, true
}

// withoutStatus returns data without the publication status. A status moves
// through the publish and unpublish routes. An update body or a restored
// revision that carried one would move it as a side effect: a client writing
// back a body it read earlier republishes a row an editor has since
// unpublished, and restoring an older revision publishes a draft. A create
// keeps the key, so an entry can start as a draft.
func withoutStatus(data map[string]any) map[string]any {
	if _, ok := data[core.ContentStatusKey]; !ok {
		return data
	}
	out := maps.Clone(data)
	delete(out, core.ContentStatusKey)
	return out
}

// isPublished reports whether a row is public. See core.ContentPublished.
func isPublished(data map[string]any) bool {
	return core.ContentPublished(data)
}

// canReadDrafts reports whether the caller may see unpublished rows of a
// schema, on the terms core.CanReadDrafts sets for every transport.
func (h *ContentHandler) canReadDrafts(r *http.Request, schema string) bool {
	return core.CanReadDrafts(r.Context(), h.checker(), requestClaims(r), schema)
}

// draftResponse reports whether this response may carry drafts of schema,
// and when it may, marks it private. A response that can hold drafts belongs
// to the caller alone: served public with a max-age, a shared cache keyed on
// anything but the session would hand an editor's drafts to the next reader.
func (h *ContentHandler) draftResponse(w http.ResponseWriter, r *http.Request, schema string) bool {
	if !h.canReadDrafts(r, schema) {
		return false
	}
	w.Header().Set(apimw.PrivateResponseHeader, "1")
	return true
}

// checkStatusFilter refuses a list filter on any status but published from a
// caller who cannot read drafts, answering 403. The list serves only
// published rows unless the filter names a status, so without this check
// the filter is a way around that default.
func (h *ContentHandler) checkStatusFilter(w http.ResponseWriter, r *http.Request, schema string, filters map[string]any) bool {
	v, ok := filters["_status"]
	if !ok || v == core.ContentStatusPublished || h.canReadDrafts(r, schema) {
		return true
	}
	httpx.ErrorReq(w, r, http.StatusForbidden, "permission denied")
	return false
}

// refuseHiddenFilters answers 403 when a filter, a range or a sort names a
// field the caller's mask hides, and returns false after answering. The mask
// keeps the field out of the response, but a filter on it still says which
// entries hold a guessed value, so it reads the field one guess at a time,
// and a sort or a range reads its order.
func (h *ContentHandler) refuseHiddenFilters(w http.ResponseWriter, r *http.Request, schema string, columns []string) bool {
	if len(columns) == 0 {
		return true
	}
	hidden, err := h.hiddenFields(r, schema)
	switch {
	case errors.Is(err, errNoCaller):
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
		return false
	case err != nil:
		httpx.StoreError(w, r, err, "database error")
		return false
	}
	for _, key := range columns {
		if hidden.Names(key) {
			httpx.ErrorReq(w, r, http.StatusForbidden, core.ErrHiddenFieldQueried.Error())
			return false
		}
	}
	return true
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

// rangeOps are the bounds a list filter may put on a column, written
// filters[field][gte] and so on.
var rangeOps = []string{"gte", "lte", "gt", "lt"}

// takeRanges moves each bound that parseFilters read out of filters and into
// the ranges it answers. Any other bracketed key stays a filter, which the
// store refuses as an unknown column.
func takeRanges(filters map[string]any) map[string]db.ListRange {
	var ranges map[string]db.ListRange
	for key, v := range filters {
		for _, op := range rangeOps {
			col, ok := strings.CutSuffix(key, "]["+op)
			if !ok || col == "" {
				continue
			}
			if ranges == nil {
				ranges = map[string]db.ListRange{}
			}
			ranges[col] = withBound(ranges[col], op, v)
			delete(filters, key)
		}
	}
	return ranges
}

// withBound answers bound with the side op names set to v.
func withBound(bound db.ListRange, op string, v any) db.ListRange {
	switch op {
	case "gte":
		bound.GTE = v
	case "lte":
		bound.LTE = v
	case "gt":
		bound.GT = v
	case "lt":
		bound.LT = v
	}
	return bound
}

// conditionOps are the operators a list filter may name besides equality
// and the bounds, written filters[field][op]=value.
var conditionOps = []string{db.OpNotEqual, db.OpContains, db.OpNotContains, db.OpIn, db.OpNotIn, db.OpEmpty}

// maxConditionValues caps the values all of a list's conditions bind
// together. Fifty values on each of many fields would pass the 2100
// parameters SQL Server takes in one statement, and the list would fail
// there alone.
const maxConditionValues = 200

// takeConditions moves each filters[field][op] that parseFilters read out of
// filters. filters[field][eq] becomes the equality filters[field] is, and
// every other operator a condition. in and nin take every value of their
// key, so filters[field][in]=a&filters[field][in]=b is one condition with
// two values, and more than db.MaxListValues answers false, as do more than
// maxConditionValues across every condition. So does an eq beside an
// equality on the same field, which would be two answers to one question.
// An eq after another operator, as in filters[field][gt][eq], stays a filter
// the store refuses as an unknown column. Whether the field takes the
// operator is the store's question.
func takeConditions(query url.Values, filters map[string]any) ([]db.ListCondition, bool) {
	var conds []db.ListCondition
	total := 0
	// The keys are read before the loop moves any, so a field named like an
	// operator key is never read a second time.
	for _, key := range slices.Collect(maps.Keys(filters)) {
		v := filters[key]
		if col, ok := strings.CutSuffix(key, "][eq"); ok && col != "" && !strings.Contains(col, "][") {
			if _, both := filters[col]; both {
				return nil, false
			}
			filters[col] = v
			delete(filters, key)
			continue
		}
		for _, op := range conditionOps {
			col, ok := strings.CutSuffix(key, "]["+op)
			if !ok || col == "" {
				continue
			}
			values := []any{v}
			if op == db.OpIn || op == db.OpNotIn {
				all := query["filters["+key+"]"]
				if len(all) > db.MaxListValues {
					return nil, false
				}
				values = make([]any, len(all))
				for i, one := range all {
					values[i] = one
				}
			}
			total += len(values)
			if total > maxConditionValues {
				return nil, false
			}
			conds = append(conds, db.ListCondition{Column: col, Op: op, Values: values})
			delete(filters, key)
		}
	}
	return conds, true
}

// takePaths moves each equality and bound whose key is a path, such as
// filters[author.name]=Ann, into conds, with eq or the bound's name as its
// operator, because the store reads a path only as a condition. It answers
// false when the values of conds then pass maxConditionValues, so a path's
// values count toward the list's limit.
func takePaths(filters map[string]any, ranges map[string]db.ListRange, conds []db.ListCondition) ([]db.ListCondition, bool) {
	for key, v := range filters {
		if db.IsPath(key) {
			conds = append(conds, db.ListCondition{Column: key, Op: db.OpEqual, Values: []any{v}})
			delete(filters, key)
		}
	}
	for key, bound := range ranges {
		if !db.IsPath(key) {
			continue
		}
		for _, b := range []struct {
			op string
			v  any
		}{{"gte", bound.GTE}, {"lte", bound.LTE}, {"gt", bound.GT}, {"lt", bound.LT}} {
			if b.v != nil {
				conds = append(conds, db.ListCondition{Column: key, Op: b.op, Values: []any{b.v}})
			}
		}
		delete(ranges, key)
	}
	total := 0
	for _, c := range conds {
		total += len(c.Values)
	}
	return conds, total <= maxConditionValues
}

// pathGate answers, for each content type a filter path reaches, whether the
// caller may read it, the fields its mask hides there and whether its drafts
// count, on the terms a direct read of that type gets. A type whose drafts
// count marks the answer private, as a populated relation does.
func (h *ContentHandler) pathGate(w http.ResponseWriter, r *http.Request) db.PathGate {
	return func(_ context.Context, schema string) (func(string) bool, bool, bool, error) {
		switch err := h.authorize(r, schema, "read"); {
		case err == nil:
		case errors.Is(err, errSchemaNotAllowed), errors.Is(err, errScopeDenied), errors.Is(err, errPermissionDenied), errors.Is(err, errNoCaller):
			return nil, false, false, nil
		default:
			return nil, false, false, err
		}
		hidden, err := h.hiddenFields(r, schema)
		if err != nil {
			return nil, false, false, err
		}
		return hidden.Names, h.draftResponse(w, r, schema), true, nil
	}
}

// parseListSort reads ?sort=field, or ?sort=-field for the reverse order.
// No sort answers the zero value. An empty sort, a bare minus and a sort
// given twice answer false. Whether the field may be sorted by is the
// store's question, because it reads the schema.
func parseListSort(r *http.Request) (db.ListSort, bool) {
	vals, ok := r.URL.Query()["sort"]
	if !ok {
		return db.ListSort{}, true
	}
	if len(vals) != 1 {
		return db.ListSort{}, false
	}
	col, desc := strings.CutPrefix(vals[0], "-")
	if col == "" {
		return db.ListSort{}, false
	}
	return db.ListSort{Column: col, Desc: desc}, true
}

// queriedColumns answers every column a list query reads a value of: each
// filter, each range, each condition and the sort.
func queriedColumns(filters map[string]any, ranges map[string]db.ListRange, conds []db.ListCondition, sort db.ListSort) []string {
	out := make([]string, 0, len(filters)+len(ranges)+len(conds)+1)
	for col := range filters {
		out = append(out, col)
	}
	for col := range ranges {
		out = append(out, col)
	}
	for _, c := range conds {
		out = append(out, c.Column)
	}
	if sort.Column != "" {
		out = append(out, sort.Column)
	}
	return out
}

// storeError answers a failed store call and, when the answer is a 5xx, writes
// the driver error to the log. The message on the wire is deliberately static,
// so without this the whole class of engine-side content failures would reach
// the caller as "failed to create content" and the operator as nothing at all:
// a 503 in the access log with no line anywhere saying why.
func storeError(w http.ResponseWriter, r *http.Request, err error, msg string) {
	if relationRefused(w, r, err) {
		return
	}
	// An install with no schema engine says so, on every route, rather than
	// letting the absence read as a missing content type or an empty page.
	if errors.Is(err, core.ErrNoSchemaEngine) {
		httpx.ErrorReq(w, r, http.StatusServiceUnavailable,
			"no schema engine is installed, so the engine cannot serve content")
		return
	}
	status := httpStatus(err)
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "content: "+msg, "schema", chi.URLParam(r, "schema"), "status", status, "err", err)
	}
	httpx.ErrorReq(w, r, status, msg)
}

// storeFailureStatus is httpStatus for a handler that answers the failure
// itself, after httpx.LogStoreFailure has recorded a 5xx, because the body
// carries none of the cause.
func storeFailureStatus(r *http.Request, err error) int {
	status := httpStatus(err)
	httpx.LogStoreFailure(r, status, err)
	return status
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
