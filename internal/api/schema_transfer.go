package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/schemaimport"
)

// Moving content types between projects.
//
// Export writes every content type as a bundle. Import reads one back, from
// this engine or from another system. The two are deliberately the same
// format, so an export is something that can actually be restored rather than a
// backup nobody can read.
//
// Import is a two-step operation. A dry run reports what would change,
// including the DDL, and is the default. Applying takes an explicit flag. The
// statements a schema change generates include destructive ones, and an
// operator should see those before they run rather than after.

// maxBundleBytes bounds an uploaded bundle. A schema definition is small. This
// is large enough for a project of several hundred content types and small
// enough that an unbounded upload cannot exhaust memory. A route declares the
// body it accepts because the engine's own guard runs first and is the smaller
// limit otherwise.
const maxBundleBytes = 4 << 20

// schemaImportMediaTypes are the media types the import reads besides JSON.
// An export is YAML by default, and the router admits these on the import's
// path alone, so the document declares exactly what a caller may send.
var schemaImportMediaTypes = []string{"application/yaml", "application/x-yaml", "text/yaml"}

// schemaExportHandler writes every content type as a portable bundle.
// GET /api/admin/schemas/export?format=yaml|json
func schemaExportHandler(host core.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eng := schemaEngineFrom(host)
		if eng == nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "schema engine unavailable")
			return
		}

		bundle, err := schemaimport.ExportBundle(r.Context(), eng, instanceSource(host))
		if err != nil {
			httpx.StoreError(w, r, err, "failed to export content types")
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		if strings.EqualFold(r.URL.Query().Get("format"), "json") {
			respond(w, http.StatusOK, bundle)
			return
		}

		out, err := bundle.MarshalYAML()
		if err != nil {
			httpx.ErrorReq(w, r, http.StatusInternalServerError, "failed to render content types")
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="lyeve-schemas.yaml"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// schemaImportResponse is the result of a planned or applied import.
type schemaImportResponse struct {
	Applied bool                `json:"applied"`
	Plan    schemaimport.Plan   `json:"plan"`
	Renamed map[string]string   `json:"renamed,omitempty"`
	Notes   []schemaimport.Note `json:"notes,omitempty"`
}

// bundleTooLarge is the import's answer to a body over maxBundleBytes, whether
// the size was declared or found while reading.
const bundleTooLarge = "content type definitions are too large"

// schemaNeedsTenant answers an import that names no tenant.
const schemaNeedsTenant = "schema definitions belong to a tenant, and the request names none"

// schemaImportHandler reads a bundle, or a definition from another system, and
// reports or applies what it would change.
//
// POST /api/admin/schemas/import?from=<source>&apply=true
//
// from selects the converter: lyeve (the default), json-schema, openapi,
// strapi, contentful or wordpress. Without apply=true nothing is written.
func schemaImportHandler(host core.Host) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eng := schemaEngineFrom(host)
		if eng == nil {
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "schema engine unavailable")
			return
		}

		// A definition written with no tenant in scope is stored under none,
		// so no tenant lists it or owns it. A super admin with no tenant in
		// scope is refused before the upload is read, as the schema write
		// routes refuse one.
		if _, err := core.RequireTenantID(r.Context()); err != nil {
			httpx.ErrorReq(w, r, http.StatusBadRequest, schemaNeedsTenant)
			return
		}

		// The router leaves this refusal to the handler, so a body declared
		// over the limit is refused here before any of it is read, as the
		// engine's body guards refuse one. The connection closes rather than
		// draining an upload that has already been refused.
		if r.ContentLength > maxBundleBytes {
			w.Header().Set("Connection", "close")
			httpx.ErrorReq(w, r, http.StatusRequestEntityTooLarge, bundleTooLarge)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBundleBytes))
		if err != nil {
			httpx.ErrorReq(w, r, http.StatusRequestEntityTooLarge, bundleTooLarge)
			return
		}
		if len(body) == 0 {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "no content type definitions in the request body")
			return
		}

		bundle, conv, err := convertUpload(r.URL.Query().Get("from"), body)
		if err != nil {
			// The converter's message names the file position or the field it
			// could not read, which is the whole value of reporting it.
			httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, err.Error()) //nolint:raw-error-text
			return
		}

		out := schemaImportResponse{}
		if conv != nil {
			out.Renamed, out.Notes = conv.Renamed, conv.Notes
		}

		apply := strings.EqualFold(r.URL.Query().Get("apply"), "true")
		if !apply {
			plan, err := schemaimport.PlanImport(r.Context(), eng, bundle)
			if refusedDefinition(w, r, err) {
				return
			}
			if err != nil {
				httpx.StoreError(w, r, err, "failed to plan the import")
				return
			}
			out.Plan = plan
			w.Header().Set("Cache-Control", "no-store")
			respond(w, http.StatusOK, out)
			return
		}

		plan, err := schemaimport.ApplyBundle(r.Context(), eng, bundle)
		if refusedDefinition(w, r, err) {
			return
		}
		out.Plan = plan
		if blocked := plan.Blocked(); len(blocked) > 0 {
			// A refusal to start names the schemas whose references cannot be
			// satisfied, which is what the operator has to fix. The names are
			// the upload's own.
			httpx.ErrorReq(w, r, http.StatusConflict,
				"the import references content types that are neither in the upload nor on this instance: "+strings.Join(blocked, ", "))
			return
		}
		if err != nil {
			// The engine's error wraps the database's, which names tables,
			// constraints and columns, so it goes to the log and the answer
			// is fixed.
			httpx.StoreError(w, r, err, "failed to apply the import")
			return
		}
		out.Applied = true
		w.Header().Set("Cache-Control", "no-store")
		respond(w, http.StatusOK, out)
	}
}

// refusedDefinition answers an import the schema engine refused because a
// definition in it is invalid, and reports whether it did. The engine names
// the schema and the field it could not accept, which is what the operator has
// to fix, so the message is the answer.
func refusedDefinition(w http.ResponseWriter, r *http.Request, err error) bool {
	var invalid *schemaimport.InvalidSchemaError
	if !errors.As(err, &invalid) {
		return false
	}
	httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, err.Error()) //nolint:raw-error-text
	return true
}

// convertUpload turns an upload into a bundle, through a converter when the
// definitions came from another system.
func convertUpload(from string, body []byte) (*schemaimport.Bundle, *schemaimport.Conversion, error) {
	switch strings.ToLower(strings.TrimSpace(from)) {
	case "", "lyeve", "bundle":
		b, err := schemaimport.ParseBundle(body)
		return b, nil, err
	case "json-schema", "jsonschema":
		conv, err := schemaimport.FromJSONSchema(body)
		return bundleOf(conv), conv, err
	case "openapi", "swagger":
		conv, err := schemaimport.FromOpenAPI(body)
		return bundleOf(conv), conv, err
	case "strapi":
		conv, err := schemaimport.FromStrapi(body)
		return bundleOf(conv), conv, err
	case "contentful":
		conv, err := schemaimport.FromContentful(body)
		return bundleOf(conv), conv, err
	case "wordpress", "wp", "acf":
		conv, err := schemaimport.FromWordPress(body)
		return bundleOf(conv), conv, err
	default:
		return nil, nil, fmt.Errorf("unknown source %q: use lyeve, json-schema, openapi, strapi, contentful or wordpress", from)
	}
}

func bundleOf(conv *schemaimport.Conversion) *schemaimport.Bundle {
	if conv == nil {
		return nil
	}
	return conv.Bundle
}

// schemaEngineFrom reaches the schema engine through the host, which is what
// owns it. Returns nil when the engine is not wired, which an install with no
// schema engine is.
func schemaEngineFrom(host core.Host) schemaimport.Engine {
	if host == nil {
		return nil
	}
	eng := host.Schema()
	if eng == nil {
		return nil
	}
	return eng
}

// instanceSource labels an export with the instance it came from, for a human
// reading the file later.
func instanceSource(host core.Host) string {
	if host == nil {
		return "lyeve"
	}
	cfg := host.Config()
	if cfg == nil {
		return "lyeve"
	}
	if id := cfg.String("instance_id"); id != "" {
		return "lyeve/" + id
	}
	return "lyeve"
}
