package api

import (
	"net/http"
	"strings"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// fieldsRefused is the one answer to a fields list naming a field the caller
// cannot read, whether the type has no such field or the caller's mask hides
// it, so the list cannot tell a hidden field from a missing one.
const fieldsRefused = "invalid query parameter: fields"

// entryFields are the names every entry carries outside its data. A fields
// list may name them, and the entry keeps them whatever the list says.
var entryFields = map[string]bool{"id": true, "created_at": true, "updated_at": true}

// contentFields is the set of data keys a read's ?fields= keeps. Nil keeps
// every key.
type contentFields map[string]struct{}

// fieldNames splits a fields value at its commas, and reports false when a
// name is empty.
func fieldNames(v string) ([]string, bool) {
	names := strings.Split(v, ",")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
		if names[i] == "" {
			return nil, false
		}
	}
	return names, true
}

// resolveFields resolves names against the type sc, for a caller whose mask
// hides hidden. A name is a field of the type, the column a belongs_to
// relation is stored in, a key the engine keeps in the type's data, or one of
// entryFields. A relation field keeps both its column and its populated
// record. It reports false for any other name and for a hidden one. No names
// select nothing, which keeps every key.
func resolveFields(sc *core.Schema, hidden core.HiddenFields, names []string) (contentFields, bool) {
	if len(names) == 0 {
		return nil, true
	}
	keys := map[string][]string{}
	for _, f := range sc.Fields {
		if f.System {
			continue
		}
		keys[f.Name] = append(keys[f.Name], f.Name)
		if f.FieldType == "relation" && (f.RelationType == core.RelBelongsTo || f.RelationType == "") {
			col := core.FKColumn(f.Name, f.RelationFKName)
			keys[f.Name] = append(keys[f.Name], col)
			keys[col] = []string{col}
		}
	}
	for key, on := range map[string]bool{"deleted_at": sc.WithSoftDelete, "_status": sc.WithDraftPublish, "_locale": sc.WithLocalization} {
		if on {
			keys[key] = []string{key}
		}
	}
	out := contentFields{}
	for _, name := range names {
		if entryFields[name] {
			continue
		}
		kept, known := keys[name]
		if !known || hidden.Names(name) {
			return nil, false
		}
		for _, k := range kept {
			out[k] = struct{}{}
		}
	}
	return out, true
}

// apply returns data narrowed to the selected keys. It builds a new map, so
// a map a hook or a cache still holds stays whole.
func (f contentFields) apply(data map[string]any) map[string]any {
	if f == nil {
		return data
	}
	out := make(map[string]any, len(f))
	for k := range f {
		if v, ok := data[k]; ok {
			out[k] = v
		}
	}
	return out
}

// readFields resolves a content read's ?fields= against schema and the
// fields the caller's mask hides, answering and returning false when it
// cannot. A read with no fields value keeps every key.
func (h *ContentHandler) readFields(w http.ResponseWriter, r *http.Request, schema string, hidden core.HiddenFields) (contentFields, bool) {
	v := r.URL.Query().Get("fields")
	if v == "" {
		return nil, true
	}
	names, ok := fieldNames(v)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, fieldsRefused)
		return nil, false
	}
	sc, err := h.schemas.GetByName(r.Context(), schema)
	if err == nil && sc == nil {
		err = domain.ErrNotFound
	}
	if err != nil {
		storeError(w, r, err, "failed to load schema")
		return nil, false
	}
	fields, ok := resolveFields(sc, hidden, names)
	if !ok {
		httpx.ErrorReq(w, r, http.StatusBadRequest, fieldsRefused)
		return nil, false
	}
	return fields, true
}
