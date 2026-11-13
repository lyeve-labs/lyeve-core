package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// The public delivery routes serve the published entries of the types a
// tenant marked for public delivery to anyone, with no credential, in a form
// a shared cache may keep. The answer depends on the host and the URL alone:
// the tenant comes from the host, every parameter is held to a strict
// grammar, and a request that carries a credential is refused.

// The delivery grammar's bounds. A value outside them is refused rather than
// clamped, because a clamped value is a second URL for one body.
const (
	deliveryMaxOffset      = 10000
	deliveryMaxFilters     = 4
	deliveryMaxFilterValue = 255
	deliveryMaxPopulate    = 8
	deliveryMaxDepth       = 2
)

// The delivery lifetimes past the two the configuration sets.
// stale-if-error is short so a withdrawn entry stands for at most ten minutes
// while the origin fails.
const (
	deliveryStaleWhileRevalidate = 60 * time.Second
	deliveryStaleIfError         = 600 * time.Second
)

// deliveryPreflightMaxAge is how long a browser may keep a delivery
// preflight answer, in seconds. The answer never changes with the
// configuration, so a browser may keep it as long as it allows.
const deliveryPreflightMaxAge = "86400"

// deliveryFiltersRefused is the one answer to a filter the grammar does not
// admit, whether the field is unknown, private or a relation into a type that
// is not marked, so a filter cannot tell a hidden field from a missing one.
const deliveryFiltersRefused = "invalid query parameter: filters"

// deliverySortRefused is the one answer to a sort the grammar does not admit,
// for the reason deliveryFiltersRefused is one answer.
const deliverySortRefused = "invalid query parameter: sort"

// deliveryCachePolicy is what a delivery answer is served with. The routes
// refuse every credentialed request, so the answer varies by no credential.
func deliveryCachePolicy(maxAge, sharedMaxAge time.Duration) apimw.CachePolicy {
	return apimw.CachePolicy{
		Public:               true,
		MaxAge:               maxAge,
		SharedMaxAge:         sharedMaxAge,
		StaleWhileRevalidate: deliveryStaleWhileRevalidate,
		StaleIfError:         deliveryStaleIfError,
		RefusesCredentials:   true,
	}
}

// refuseCredentials answers 400 to a delivery request that carries a
// credential. By the time a route runs, the credential's tenant has replaced
// the host's, so serving it would put one tenant's answer under another
// tenant's URL. The refusal keeps the default no-store, so no cache keeps it.
func refuseCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apimw.CarriesCredential(r) {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "the delivery routes take no credential, so send the request without one")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// deliveryCORS lets every site read a delivery answer, whatever CORS_ORIGINS
// names. A reflected origin would add Vary: Origin and split each cached
// entry per site, and a cache that ignores Vary would hand one site's header
// to another. The wildcard is safe on a path that reads no credential, and
// credentials are never allowed with it.
func deliveryCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Del("Access-Control-Allow-Credentials")
		var vary []string
		for _, v := range h.Values("Vary") {
			for _, m := range strings.Split(v, ",") {
				if m = strings.TrimSpace(m); m != "" && !strings.EqualFold(m, "Origin") {
					vary = append(vary, m)
				}
			}
		}
		h.Del("Vary")
		if len(vary) > 0 {
			h.Set("Vary", strings.Join(vary, ", "))
		}
		next.ServeHTTP(w, r)
	})
}

// deliveryPreflight answers a preflight to a delivery path for any origin,
// before the router's CORS answer, which names only the CORS_ORIGINS sites.
// It allows the two reads and the two revalidation headers, never a
// credential or a credential header, so a site can read an answer with no
// help from the operator. Every other request passes through.
func deliveryPreflight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions || !isDeliveryPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD")
		h.Set("Access-Control-Allow-Headers", "If-None-Match, If-Modified-Since")
		h.Set("Access-Control-Max-Age", deliveryPreflightMaxAge)
		w.WriteHeader(http.StatusNoContent)
	})
}

// isDeliveryPath reports whether path is one the two delivery routes serve:
// /api/v1/delivery/{schema} or /api/v1/delivery/{schema}/{id}.
func isDeliveryPath(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/v1/delivery/")
	if !ok {
		return false
	}
	segs := strings.Split(rest, "/")
	if len(segs) > 2 {
		return false
	}
	for _, seg := range segs {
		if seg == "" {
			return false
		}
	}
	return true
}

// deliveryQuery is a delivery request's parameters once the grammar admits
// them.
type deliveryQuery struct {
	limit    int
	offset   int
	filters  map[string]string
	populate domain.PopulateConfig
	fields   []string
	sort     db.ListSort
}

// parseDeliveryQuery holds the query to the delivery grammar, answering 400
// and returning false for anything outside it: an unknown or repeated
// parameter, a number out of range or not written plainly, more than four
// filters or eight populate paths, a populate path past two segments or with
// a wildcard or a field mask, a filter value over 255 characters, or a name in
// fields that deliveryFieldNames refuses. A read by id takes populate, depth
// and fields only. Whether
// each path names a relation delivery follows is checked against the
// definition, by deliveryPaths, and each name in fields by resolveFields.
func parseDeliveryQuery(w http.ResponseWriter, r *http.Request, list bool) (deliveryQuery, bool) {
	q := deliveryQuery{limit: contentListDefaultLimit, filters: map[string]string{}}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query string")
		return q, false
	}
	refuse := func(msg string) (deliveryQuery, bool) {
		httpx.ErrorReq(w, r, http.StatusBadRequest, msg)
		return q, false
	}
	for key, vals := range values {
		if len(vals) != 1 {
			return refuse("a query parameter is repeated")
		}
		v := vals[0]
		col, isFilter := strings.CutPrefix(key, "filters[")
		col, closed := strings.CutSuffix(col, "]")
		switch {
		case key == "populate":
			paths := strings.Split(v, ",")
			if len(paths) > deliveryMaxPopulate {
				return refuse("invalid query parameter: populate")
			}
			for i, p := range paths {
				paths[i] = strings.TrimSpace(p)
				if paths[i] == "" || strings.ContainsAny(paths[i], "*{}") || strings.Count(paths[i], ".") >= deliveryMaxDepth {
					return refuse("invalid query parameter: populate")
				}
			}
			q.populate.Paths = paths
		case key == "depth":
			n, ok := plainInt(v, 0, deliveryMaxDepth)
			if !ok {
				return refuse("invalid query parameter: depth")
			}
			q.populate.MaxDepth = n
		case key == "fields":
			names, ok := deliveryFieldNames(v)
			if !ok {
				return refuse(fieldsRefused)
			}
			q.fields = names
		case list && key == "limit":
			n, ok := plainInt(v, contentListMinLimit, contentListMaxLimit)
			if !ok {
				return refuse("invalid query parameter: limit")
			}
			q.limit = n
		case list && key == "offset":
			n, ok := plainInt(v, 0, deliveryMaxOffset)
			if !ok {
				return refuse("invalid query parameter: offset")
			}
			q.offset = n
		case list && key == "sort":
			col, desc := strings.CutPrefix(v, "-")
			if col == "" {
				return refuse(deliverySortRefused)
			}
			q.sort = db.ListSort{Column: col, Desc: desc}
		case list && isFilter && closed && col != "":
			if len(v) > deliveryMaxFilterValue || len(q.filters) == deliveryMaxFilters {
				return refuse(deliveryFiltersRefused)
			}
			q.filters[col] = v
		default:
			return refuse("unknown query parameter")
		}
	}
	return q, true
}

// deliveryFieldNames reads a fields value under the delivery grammar. A name
// that is empty or padded with space, a name given twice and a name every
// entry carries anyway are refused, because each would be a second URL for
// one body. The order stays free, as populate's does.
func deliveryFieldNames(v string) ([]string, bool) {
	names := strings.Split(v, ",")
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "" || strings.TrimSpace(n) != n || entryFields[n] || seen[n] {
			return nil, false
		}
		seen[n] = true
	}
	return names, true
}

// plainInt reads v as a whole number from min to max, written the way
// strconv writes it, so "05" and "+5" are not a second spelling of 5.
func plainInt(v string, min, max int) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(n) != v || n < min || n > max {
		return 0, false
	}
	return n, true
}

// deliveryReader resolves, once per request, which types the request's
// tenant serves on delivery and what of each is hidden.
type deliveryReader struct {
	h      *ContentHandler
	ctx    context.Context
	open   map[string]*core.Schema
	hidden map[string]core.HiddenFields
}

func (h *ContentHandler) deliveryReader(ctx context.Context) *deliveryReader {
	return &deliveryReader{h: h, ctx: ctx, open: map[string]*core.Schema{}, hidden: map[string]core.HiddenFields{}}
}

// schema returns the tenant's definition of name when delivery serves it,
// and nil when it does not: no such type, a name spelled otherwise than it
// is stored, a type its tenant has not marked, or one REST does not read. A
// definition that cannot be read is an error, so the path fails closed.
func (d *deliveryReader) schema(name string) (*core.Schema, error) {
	if sc, seen := d.open[name]; seen {
		return sc, nil
	}
	sc, err := d.h.schemas.GetByName(d.ctx, name)
	switch {
	case errors.Is(err, core.ErrSchemaNotFound) || errors.Is(err, domain.ErrNotFound):
		sc = nil
	case err != nil:
		return nil, fmt.Errorf("delivery schema %s: %w", name, err)
	case sc == nil || sc.Name != name || !sc.PublicDelivery || !sc.Transports.CanRead(core.TransportREST):
		sc = nil
	}
	d.open[name] = sc
	return sc, nil
}

// hiddenFields is what delivery leaves out of an entry of sc: every field
// marked private and every relation into a type delivery does not serve,
// with a belongs_to relation's column and populated record hidden together.
func (d *deliveryReader) hiddenFields(sc *core.Schema) (core.HiddenFields, error) {
	if hidden, seen := d.hidden[sc.Name]; seen {
		return hidden, nil
	}
	hidden := core.HiddenFields{}
	for _, f := range sc.Fields {
		switch {
		case f.Private:
			hidden[f.Name] = struct{}{}
		case f.FieldType == "relation":
			target, err := d.schema(f.RelationTo)
			if err != nil {
				return nil, err
			}
			if target == nil {
				hidden[f.Name] = struct{}{}
			}
		}
	}
	hidden = hidden.WithRelations(sc.Fields)
	d.hidden[sc.Name] = hidden
	return hidden, nil
}

// deliveryOrdered answers the field col names when delivery may sort or
// bound by it: created_at, updated_at, and a number, integer, decimal, date
// or datetime field it serves. A private field answers as a missing one.
func deliveryOrdered(sc *core.Schema, hidden core.HiddenFields, col string) (core.SchemaField, bool) {
	if col == "created_at" || col == "updated_at" {
		return core.SchemaField{Name: col, FieldType: "datetime"}, true
	}
	for _, f := range sc.Fields {
		if f.Name != col || f.System || hidden.Names(f.Name) {
			continue
		}
		switch f.FieldType {
		case "number", "integer", "decimal", "date", "datetime":
			return f, true
		}
	}
	return core.SchemaField{}, false
}

// deliveryRange admits filters[field][gte] and [lte] on a field
// deliveryOrdered answers, with a value of that field's form, and answers
// the field's column. The value goes to the store as written, which parses
// it by the same type.
func deliveryRange(sc *core.Schema, hidden core.HiddenFields, key, v string) (string, string, bool) {
	for _, op := range rangeOps {
		col, cut := strings.CutSuffix(key, "]["+op)
		if !cut {
			continue
		}
		f, ok := deliveryOrdered(sc, hidden, col)
		if !ok {
			return "", "", false
		}
		if f.FieldType == "number" {
			n, err := strconv.ParseFloat(v, 64)
			return col, op, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
		}
		_, ok = deliveryFilterValue(f, v)
		return col, op, ok
	}
	return "", "", false
}

// deliveryFilters admits a filter on id and on the column of each field
// delivery serves whose values it can check, with a value of that field's
// form, and a range deliveryRange admits, and refuses everything else with
// the one answer an unknown field gets. A checked value goes to the query in
// the form its column compares, so no dialect sees a value its column cannot
// hold.
func deliveryFilters(sc *core.Schema, hidden core.HiddenFields, asked map[string]string) (map[string]any, map[string]db.ListRange, bool) {
	if len(asked) == 0 {
		return nil, nil, true
	}
	columns := map[string]core.SchemaField{"id": {Name: "id", FieldType: "uid"}}
	for _, f := range sc.Fields {
		if hidden.Names(f.Name) {
			continue
		}
		switch {
		case f.FieldType != "relation":
			columns[f.Name] = f
		case f.RelationType == core.RelBelongsTo || f.RelationType == "":
			columns[core.FKColumn(f.Name, f.RelationFKName)] = f
		}
	}
	out := make(map[string]any, len(asked))
	var ranges map[string]db.ListRange
	for col, v := range asked {
		if strings.Contains(col, "][") {
			rcol, op, ok := deliveryRange(sc, hidden, col, v)
			if !ok {
				return nil, nil, false
			}
			if ranges == nil {
				ranges = map[string]db.ListRange{}
			}
			bound := ranges[rcol]
			if op == "gte" {
				bound.GTE = v
			} else {
				bound.LTE = v
			}
			ranges[rcol] = bound
			continue
		}
		f, ok := columns[col]
		if !ok || hidden.Names(col) {
			return nil, nil, false
		}
		if out[col], ok = deliveryFilterValue(f, v); !ok {
			return nil, nil, false
		}
	}
	return out, ranges, true
}

// deliveryFilterValue reads v as a value of f, in the forms the content API
// takes: a canonical UUID for an id or a relation, an integer, a decimal
// within the field's precision and scale, true or false, a date written
// 2006-01-02, and a datetime in any form a write accepts. Text, email and URL
// fields take any value. A field of any other type is not filterable here.
func deliveryFilterValue(f core.SchemaField, v string) (any, bool) {
	switch f.FieldType {
	case "text", "email", "url":
		return v, true
	case "uid", "relation":
		id, err := uuid.Parse(v)
		return v, err == nil && id.String() == v
	case "integer":
		n, err := core.IntegerValue(v)
		return n, err == nil
	case "decimal":
		p, scale := core.DecimalSpec(f)
		text, err := core.DecimalValue(v, p, scale)
		return text, err == nil
	case "boolean":
		return v == "true", v == "true" || v == "false"
	case "date":
		_, err := time.Parse(time.DateOnly, v)
		return v, err == nil
	case "datetime":
		return db.ParseContentTime(v)
	}
	return nil, false
}

// deliveryPaths answers 400 and returns false unless every populate path
// walks only belongs_to relations delivery serves: each segment names a
// relation of the type the walk has reached, held on that type's entry, not
// hidden there, into a type delivery serves. A definition that cannot be read
// answers 503.
func (h *ContentHandler) deliveryPaths(w http.ResponseWriter, r *http.Request, d *deliveryReader, sc *core.Schema, paths []string) bool {
	served, err := d.servedPaths(sc, paths)
	if err != nil {
		storeError(w, r, err, "content delivery is unavailable")
		return false
	}
	if !served {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: populate")
		return false
	}
	return true
}

// servedPaths reports whether every path walks only belongs_to relations
// delivery serves, as deliveryPaths describes.
func (d *deliveryReader) servedPaths(sc *core.Schema, paths []string) (bool, error) {
	for _, path := range paths {
		at := sc
		for _, seg := range strings.Split(path, ".") {
			hidden, err := d.hiddenFields(at)
			if err != nil {
				return false, err
			}
			f, ok := toOneRelation(at, seg)
			if !ok || hidden.Names(seg) {
				return false, nil
			}
			if at, err = d.schema(f.RelationTo); err != nil || at == nil {
				return false, err
			}
		}
	}
	return true, nil
}

// toOneRelation answers the belongs_to relation of sc named name.
func toOneRelation(sc *core.Schema, name string) (core.SchemaField, bool) {
	for _, f := range sc.Fields {
		if f.Name == name && f.FieldType == "relation" && (f.RelationType == core.RelBelongsTo || f.RelationType == "") {
			return f, true
		}
	}
	return core.SchemaField{}, false
}

// deliveryTarget resolves the schema a delivery request names, answering and
// returning false when it cannot be served: 404 for a request with no tenant
// and for a type delivery does not serve, 503 when a definition cannot be
// read.
func (h *ContentHandler) deliveryTarget(w http.ResponseWriter, r *http.Request, d *deliveryReader) (*core.Schema, core.HiddenFields, bool) {
	if core.TenantIDFromCtx(r.Context()) == "" {
		httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
		return nil, nil, false
	}
	sc, err := d.schema(chi.URLParam(r, "schema"))
	if err == nil && sc == nil {
		httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
		return nil, nil, false
	}
	var hidden core.HiddenFields
	if err == nil {
		hidden, err = d.hiddenFields(sc)
	}
	if err != nil {
		storeError(w, r, err, "content delivery is unavailable")
		return nil, nil, false
	}
	return sc, hidden, true
}

// deliveryBeforeRequest fires the BeforeRequest hook a content read fires.
func (h *ContentHandler) deliveryBeforeRequest(w http.ResponseWriter, r *http.Request, schema, method string) bool {
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

// deliveryPopulate resolves populate and depth over belongs_to relations
// only, each put to the delivery gate: a to-many relation and a target
// delivery does not serve are left out, and a target it serves comes back
// with its own hidden fields left out and as null when it is not published.
// A to-many relation would pull every entry it reaches, with no bound. A
// definition or a relation that cannot be read answers 503, never an answer
// with the relation left empty.
func (h *ContentHandler) deliveryPopulate(w http.ResponseWriter, r *http.Request, d *deliveryReader, items []*domain.Content, cfg domain.PopulateConfig) bool {
	if cfg.IsEmpty() || len(items) == 0 {
		return true
	}
	var gateErr error
	gate := func(_ context.Context, f domain.SchemaField) (func(map[string]any) map[string]any, bool) {
		if f.RelationType != core.RelBelongsTo && f.RelationType != "" {
			return nil, false
		}
		sc, err := d.schema(f.RelationTo)
		if err != nil || sc == nil {
			gateErr = errors.Join(gateErr, err)
			return nil, false
		}
		hidden, err := d.hiddenFields(sc)
		if err != nil {
			gateErr = errors.Join(gateErr, err)
			return nil, false
		}
		return publishedOnly(hidden.Apply), true
	}
	err := h.store.PopulateStrict(r.Context(), items, cfg, gate)
	if err = errors.Join(gateErr, err); err != nil {
		storeError(w, r, err, "content delivery is unavailable")
		return false
	}
	return true
}

// DeliveryList serves the published entries of a type its tenant marked for
// public delivery.
// GET /api/v1/delivery/{schema}
func (h *ContentHandler) DeliveryList(w http.ResponseWriter, r *http.Request) {
	q, ok := parseDeliveryQuery(w, r, true)
	if !ok {
		return
	}
	d := h.deliveryReader(r.Context())
	sc, hidden, ok := h.deliveryTarget(w, r, d)
	if !ok {
		return
	}
	filters, ranges, ok := deliveryFilters(sc, hidden, q.filters)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, deliveryFiltersRefused)
		return
	}
	if q.sort.Column != "" {
		if _, ok := deliveryOrdered(sc, hidden, q.sort.Column); !ok {
			httpx.ErrorReq(w, r, http.StatusBadRequest, deliverySortRefused)
			return
		}
	}
	if !h.deliveryPaths(w, r, d, sc, q.populate.Paths) {
		return
	}
	fields, ok := resolveFields(sc, hidden, q.fields)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, fieldsRefused)
		return
	}
	if !h.deliveryBeforeRequest(w, r, sc.Name, "list") {
		return
	}
	items, err := h.store.ListPage(r.Context(), sc.Name, db.ListQuery{
		Limit: q.limit, Offset: q.offset, Filters: filters, Ranges: ranges, Sort: q.sort,
	})
	if err != nil {
		storeError(w, r, err, "failed to list content")
		return
	}
	if !h.deliveryPopulate(w, r, d, items, q.populate) {
		return
	}
	for _, item := range items {
		item.Data = hidden.Apply(item.Data)
		h.runAfterResponse(r, sc.Name, "list", item.Data)
		item.Data = fields.apply(item.Data)
	}
	respond(w, http.StatusOK, items)
}

// DeliveryGet serves one published entry of a type its tenant marked for
// public delivery. An entry that is not published, and an id not written in
// its canonical form, answer what a missing one does, so an entry has one
// URL.
// GET /api/v1/delivery/{schema}/{id}
func (h *ContentHandler) DeliveryGet(w http.ResponseWriter, r *http.Request) {
	q, ok := parseDeliveryQuery(w, r, false)
	if !ok {
		return
	}
	d := h.deliveryReader(r.Context())
	sc, hidden, ok := h.deliveryTarget(w, r, d)
	if !ok {
		return
	}
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
		return
	}
	if !h.deliveryPaths(w, r, d, sc, q.populate.Paths) {
		return
	}
	fields, ok := resolveFields(sc, hidden, q.fields)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, fieldsRefused)
		return
	}
	if !h.deliveryBeforeRequest(w, r, sc.Name, "get") {
		return
	}
	item, err := h.store.GetByID(r.Context(), sc.Name, id)
	if err == nil && !isPublished(item.Data) {
		err = domain.ErrNotFound
	}
	if err != nil {
		if httpStatus(err) == http.StatusNotFound {
			httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
			return
		}
		storeError(w, r, err, "failed to get content")
		return
	}
	if !h.deliveryPopulate(w, r, d, []*domain.Content{item}, q.populate) {
		return
	}
	item.Data = hidden.Apply(item.Data)
	h.runAfterResponse(r, sc.Name, "get", item.Data)
	item.Data = fields.apply(item.Data)
	respond(w, http.StatusOK, item)
}
