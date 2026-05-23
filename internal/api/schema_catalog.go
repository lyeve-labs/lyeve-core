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
type schemaCatalog struct {
	source core.SchemaSource
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

// List answers every content type the caller's tenant can read.
// GET /api/v1/schemas
func (h *schemaCatalog) List(w http.ResponseWriter, r *http.Request) {
	if h.noEngine(w, r) {
		return
	}
	schemas, err := h.source.List(r.Context())
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to list schemas")
		return
	}
	out := make([]schemaCatalogResponse, 0, len(schemas))
	for _, sc := range schemas {
		out = append(out, schemaCatalogEntry(sc))
	}
	respond(w, http.StatusOK, out)
}

// Get answers one content type by name.
// GET /api/v1/schemas/{name}
func (h *schemaCatalog) Get(w http.ResponseWriter, r *http.Request) {
	if h.noEngine(w, r) {
		return
	}
	sc, err := h.source.GetByName(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.ErrorReq(w, r, http.StatusNotFound, "schema not found")
			return
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to load schema")
		return
	}
	respond(w, http.StatusOK, schemaCatalogEntry(sc))
}
