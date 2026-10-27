package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// ListRelations returns paginated items related via a many_to_many field.
// GET /api/v1/content/{schema}/{id}/relations/{field}
//
// The items belong to the field's target schema, so the caller needs read on
// both, and the target's field mask applies to what is returned. The field
// itself belongs to the source schema, so a field the source's mask hides
// answers 403: the items say which records the hidden field links to.
func (h *ContentHandler) ListRelations(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.checkPermission(w, r, schemaName, "read") {
		return
	}
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	fieldName := chi.URLParam(r, "field")
	hidden, err := core.ContentHiddenFields(r.Context(), h.checker(), requestClaims(r), schemaName)
	switch {
	case errors.Is(err, errNoCaller):
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
		return
	case err != nil:
		httpx.StoreError(w, r, err, "database error")
		return
	case hidden.Names(fieldName):
		httpx.ErrorReq(w, r, http.StatusForbidden, "permission denied")
		return
	}
	limit, err := reqparse.QueryInt(r, "limit", contentListDefaultLimit)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: limit")
		return
	}
	offset, err := reqparse.QueryOffset(r)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid query parameter: offset")
		return
	}

	limit = clampLimit(limit, contentListMinLimit, contentListMaxLimit)

	f, err := h.relationField(r, schemaName, fieldName)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid relation field")
		return
	}
	if !h.checkPermission(w, r, f.RelationTo, "read") {
		return
	}
	mask, maskOK := h.maskFor(w, r, f.RelationTo)
	if !maskOK {
		return
	}

	// The relations of a row the caller cannot read are not theirs to list.
	if !h.draftResponse(w, r, schemaName) {
		parent, err := h.store.GetByID(r.Context(), schemaName, id)
		if err != nil {
			storeError(w, r, err, "failed to list related")
			return
		}
		if !isPublished(parent.Data) {
			httpx.ErrorReq(w, r, http.StatusNotFound, "not found")
			return
		}
	}

	items, total, err := h.store.ListRelated(r.Context(), schemaName, id, f, limit, offset, h.draftResponse(w, r, f.RelationTo))
	if err != nil {
		httpx.ErrorReq(w, r, httpStatus(err), "failed to list related")
		return
	}
	for _, item := range items {
		item.Data = mask(item.Data)
	}
	respond(w, http.StatusOK, map[string]any{"data": items, "total": total})
}

// SetRelations replaces all many_to_many pivot rows for (schema, id, field).
// PUT /api/v1/content/{schema}/{id}/relations/{field}
// Body: {"ids": ["uuid1", "uuid2"]}
func (h *ContentHandler) SetRelations(w http.ResponseWriter, r *http.Request) {
	schemaName := chi.URLParam(r, "schema")
	if !h.checkPermission(w, r, schemaName, "update") {
		return
	}
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id")
		return
	}
	fieldName := chi.URLParam(r, "field")

	var body struct {
		IDs []string `json:"ids"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}

	f, err := h.relationField(r, schemaName, fieldName)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid relation field")
		return
	}

	targetIDs := make([]uuid.UUID, 0, len(body.IDs))
	for _, s := range body.IDs {
		tid, err := uuid.Parse(s)
		if err != nil {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid id in ids: "+s)
			return
		}
		targetIDs = append(targetIDs, tid)
	}

	if err := h.store.SetRelations(r.Context(), schemaName, id, f, targetIDs); err != nil {
		if !relationRefused(w, r, err) {
			httpx.ErrorReq(w, r, httpStatus(err), "failed to set relations")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// relationField looks up a SchemaField by name and validates it is a many_to_many relation.
func (h *ContentHandler) relationField(r *http.Request, schemaName, fieldName string) (domain.SchemaField, error) {
	sc, err := h.schemas.GetByName(r.Context(), schemaName)
	if err != nil {
		return domain.SchemaField{}, err
	}
	for _, f := range sc.Fields {
		if f.Name == fieldName && f.FieldType == "relation" && f.RelationType == domain.RelManyToMany {
			return f, nil
		}
	}
	return domain.SchemaField{}, domain.ErrNotFound
}
