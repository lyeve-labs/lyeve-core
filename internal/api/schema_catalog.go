package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// schemaCatalog serves the content API's read-only view of the content types
// this install can describe. The authoring routes belong to whichever plugin
// registered the schema engine, and these do not: a client reading content
// has to be able to discover what it may read without holding admin rights.
//
// It reads core.SchemaSource per request rather than holding one, because the
// engine is registered by a plugin that starts after this router is built.
//
// A content type is described only to a caller who may read its entries: the
// credential's schema list, a key's content scopes and the roles' read rule
// decide, exactly as they do on the content routes. A definition names every
// field, so listing a schema the caller cannot read tells it what the schema
// holds.
type schemaCatalog struct {
	source core.SchemaSource
	perms  core.PermissionCheckerProvider // nil = the kernel's role-only checker
}

// readable reports whether the caller may read entries of schema. A refusal
// is false with a nil error, and the error is the rule store failing.
func (h *schemaCatalog) readable(r *http.Request, schema string) (bool, error) {
	switch err := core.AuthorizeContent(r.Context(), core.PermissionCheckerOf(h.perms), requestClaims(r), schema, "read"); {
	case err == nil:
		return true, nil
	case errors.Is(err, core.ErrSchemaNotAllowed), errors.Is(err, core.ErrScopeDenied), errors.Is(err, core.ErrPermissionDenied):
		return false, nil
	default:
		return false, err
	}
}

// refuseCatalog answers a caller the catalog will not describe a schema to, in the
// words the content routes use.
func refuseCatalog(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNoCaller):
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "authentication required")
	case errors.Is(err, core.ErrSchemaNotAllowed):
		httpx.ErrorReq(w, r, http.StatusForbidden, "schema not allowed for this credential")
	case errors.Is(err, core.ErrScopeDenied):
		httpx.ErrorReq(w, r, http.StatusForbidden, "insufficient scope")
	case errors.Is(err, core.ErrPermissionDenied):
		httpx.ErrorReq(w, r, http.StatusForbidden, "permission denied")
	default:
		httpx.StoreError(w, r, err, "database error")
	}
}

// schemaDeprecation names an option a definition sets that something else
// supersedes, so a client reading the catalog learns it without reading a
// release note.
type schemaDeprecation struct {
	Option  string `json:"option"`
	Message string `json:"message"`
}

var withLocalizationDeprecated = schemaDeprecation{
	Option:  "with_localization",
	Message: "with_localization is superseded by translations: content reads take a locale and a registered localizer merges the entry's translation. The _locale column stays where it exists and the flag is still accepted, but new schemas should not set it.",
}

// schemaCatalogResponse is one content type as the content API states it.
// The definition is the engine's own, and the deprecations list says which
// options it sets that something else supersedes.
type schemaCatalogResponse struct {
	*domain.Schema
	Deprecations []schemaDeprecation `json:"deprecations,omitempty"`
}

func schemaCatalogEntry(sc *domain.Schema) schemaCatalogResponse {
	out := schemaCatalogResponse{Schema: sc}
	if sc != nil && sc.WithLocalization {
		out.Deprecations = []schemaDeprecation{withLocalizationDeprecated}
	}
	return out
}

// noEngine answers the 503 that says this install cannot describe content
// types, rather than an empty list, which says it has none. The two call for
// opposite actions and a client cannot tell them apart from a 200.
func (h *schemaCatalog) noEngine(w http.ResponseWriter, r *http.Request) bool {
	if core.NoSchemaEngine(h.source) {
		httpx.ErrorReq(w, r, http.StatusServiceUnavailable,
			"this install has no schema engine, so it cannot describe content types")
		return true
	}
	return false
}

// List answers every content type of the caller's tenant that the caller may
// read. A schema it may not read is left out, not refused, so the list says
// what this caller can reach.
// GET /api/v1/schemas
func (h *schemaCatalog) List(w http.ResponseWriter, r *http.Request) {
	if h.noEngine(w, r) {
		return
	}
	if requestClaims(r) == nil {
		refuseCatalog(w, r, core.ErrNoCaller)
		return
	}
	schemas, err := h.source.List(r.Context())
	if err != nil {
		httpx.StoreError(w, r, err, "failed to list schemas")
		return
	}
	out := make([]schemaCatalogResponse, 0, len(schemas))
	for _, sc := range schemas {
		if sc == nil {
			continue
		}
		ok, err := h.readable(r, sc.Name)
		if err != nil {
			refuseCatalog(w, r, err)
			return
		}
		if ok {
			out = append(out, schemaCatalogEntry(sc))
		}
	}
	respond(w, http.StatusOK, out)
}

// Get answers one content type by name. The caller's access is decided
// before the definition is looked up, so a refused name answers the same
// whether or not a schema by that name exists.
// GET /api/v1/schemas/{name}
func (h *schemaCatalog) Get(w http.ResponseWriter, r *http.Request) {
	if h.noEngine(w, r) {
		return
	}
	name := chi.URLParam(r, "name")
	if err := core.AuthorizeContent(r.Context(), core.PermissionCheckerOf(h.perms), requestClaims(r), name, "read"); err != nil {
		refuseCatalog(w, r, err)
		return
	}
	sc, err := h.source.GetByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.ErrorReq(w, r, http.StatusNotFound, "schema not found")
			return
		}
		httpx.StoreError(w, r, err, "failed to load schema")
		return
	}
	respond(w, http.StatusOK, schemaCatalogEntry(sc))
}
