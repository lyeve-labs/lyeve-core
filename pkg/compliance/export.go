package compliance

import (
	"context"
	"reflect"
	"sync"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Subject export (GDPR Art.15 right of access / Art.20 data portability)
//
// ## Scoping
//
// The identifier is NOT globally unique. An email address can name different
// people in two tenants, so "super_admin gate + unique identifier =
// platform-wide semantics" does not hold: it produces a bundle that hands one
// controller another person's rows.
//
// Every tenant-scoped exporter reads the tenant from the context and falls back
// to "default". A super_admin carries no tenant claim, so an unscoped export
// runs entirely against "default" while the subject's rows sit elsewhere, and
// nothing errors. The bundle has to report that gap itself.
//
// There are two supported shapes. RunSubjectExport covers one tenant, whichever
// the context names. RunSubjectExportAcrossTenants sweeps a caller-supplied list
// and keys the result by tenant so nothing merges silently. Both populate Scope,
// and both set Incomplete when the bundle is short.

// SubjectExporter is an interface that plugins implement to support
// GDPR Art.15 (Right of Access) and Art.20 (Data Portability). Plugins
// register their exporters during Start() via RegisterSubjectExporter.
// The coordinator fans out export requests to every registered exporter
// and merges the results into a portable JSON bundle.
//
// The identifier is plugin-defined: an email address, user ID (UUID string),
// IP address, or any other key that identifies the data subject's records
// within the plugin's stores.
//
// Return value is a map suitable for JSON serialization. The key is the
// plugin's section name (e.g. "plugin_<name>_<table>") and the value
// is the exported data (typically []map[string]any). Return nil map
// if no records match: this is not an error.
type SubjectExporter interface {
	// ExportSubject returns all PII data for the given identifier as a
	// JSON-serializable map. The map keys are section names (table or
	// logical grouping names). Values are slices of row data.
	//
	// Implementations must handle the case where no matching records
	// exist: return (nil, nil), not an error.
	ExportSubject(ctx context.Context, identifier string) (map[string]any, error)
}

// SubjectExporterRegistry holds registered exporters. Safe for concurrent use.
type SubjectExporterRegistry struct {
	mu        sync.Mutex
	exporters []SubjectExporter
}

// Register adds an exporter. Nil exporters are silently ignored, and one the
// registry already holds is ignored too, so a plugin may register again when
// reconfigured. Without that, each registration would put the subject's rows
// into the export once more. The eraser registry deduplicates the same way.
//
// An exporter whose dynamic type cannot be compared is appended without the
// check rather than panicking on it. Every registered exporter is a pointer,
// so this guards a future one rather than a live case.
func (r *SubjectExporterRegistry) Register(e SubjectExporter) {
	if e == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if reflect.TypeOf(e).Comparable() {
		for _, existing := range r.exporters {
			if existing == e {
				return
			}
		}
	}
	r.exporters = append(r.exporters, e)
}

// Exporters returns a snapshot copy of registered exporters.
func (r *SubjectExporterRegistry) Exporters() []SubjectExporter {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SubjectExporter, len(r.exporters))
	copy(out, r.exporters)
	return out
}

// Len returns the number of registered exporters.
func (r *SubjectExporterRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.exporters)
}

var globalExporterRegistry = &SubjectExporterRegistry{}

// RegisterSubjectExporter registers a plugin's exporter on the global registry.
// Plugins call this during Start() to advertise their export capability.
func RegisterSubjectExporter(e SubjectExporter) {
	globalExporterRegistry.Register(e)
}

// SubjectExportResult holds the merged export from all plugins.
type SubjectExportResult struct {
	// Identifier is the data subject identifier used for the query.
	Identifier string `json:"identifier"`

	// Plugins maps plugin section names to their exported data.
	// Each value is a JSON-serializable map from ExportSubject.
	Plugins map[string]any `json:"plugins"`

	// Incomplete reports that at least one exporter failed, so the bundle is
	// missing data the subject is entitled to. Without it a caller would have
	// to notice a non-empty Errors array to tell a partial export from a whole
	// one. A partial export that reads as complete is the failure mode that
	// matters: the subject is told this is everything held about them when it
	// is not.
	Incomplete bool `json:"incomplete"`

	// Tenants holds one entry per tenant when the export swept more than one.
	// Empty for a single-tenant export. Kept separate from Plugins rather than
	// merged into it: the same identifier can name different people in two
	// tenants, so a flat merge hands one controller another person's records.
	Tenants map[string]*TenantExport `json:"tenants,omitempty"`

	// MatchedTenants names every tenant that returned at least one record.
	// More than one entry is the case a controller has to resolve by hand
	// before releasing the bundle, because it may be two different people.
	MatchedTenants []string `json:"matched_tenants,omitempty"`

	// Scope records what the bundle actually covered. Always populated: a
	// response that does not say what it looked at cannot be checked.
	Scope ExportScope `json:"scope"`

	// Summary holds aggregate statistics.
	Summary ExportSummary `json:"summary"`

	// Errors holds non-fatal errors from individual exporters.
	// The export continues even if some plugins fail. Always serialized (even when
	// empty) as [] so API consumers get a stable array shape, never a missing key.
	Errors []ExportError `json:"errors"`
}

// ExportSummary holds aggregate statistics for an export operation.
type ExportSummary struct {
	// TotalRecords is the sum of all records across all plugins.
	TotalRecords int `json:"total_records"`

	// PluginsQueried is the number of exporters that were called.
	PluginsQueried int `json:"plugins_queried"`

	// PluginsWithData is the number of exporters that returned non-nil data.
	PluginsWithData int `json:"plugins_with_data"`
}

// ExportError records a non-fatal error from a single exporter.
type ExportError struct {
	// Index is the exporter's position in the registry.
	Index int `json:"index"`

	// Error is the error message.
	Error string `json:"error"`
}

// RunSubjectExport fans out the given identifier to all registered exporters.
// Each exporter is called independently. A failure in one does not stop the
// others. The results are merged into a single SubjectExportResult.
func RunSubjectExport(ctx context.Context, identifier string) (*SubjectExportResult, error) {
	per := runExportersInto(ctx, identifier)

	// A caller that resolved no tenant covered no tenant. ExportScope's own
	// contract is that Requested and Covered differ when a tenant could not be
	// scoped, and that this is what makes a bundle incomplete.
	//
	// The caller who gets here is a super_admin holding no tenant claim, which
	// the tenancy middleware deliberately lets through. Every exporter then
	// runs on the empty scope and returns nothing, and a controller answering
	// an Art.15 request is handed a bundle that says it is complete. Reporting
	// the gap is the whole remedy: an export that is honestly incomplete can be
	// re-run against a scope, and one that lies about it cannot be noticed.
	tenant := core.TenantIDFromCtx(ctx)
	scope := ExportScope{Mode: "tenant", Tenants: []string{}, Requested: 1, Covered: 0}
	if tenant != "" {
		scope.Tenants = append(scope.Tenants, tenant)
		scope.Covered = 1
	}

	result := &SubjectExportResult{
		Identifier: identifier,
		Plugins:    per.Plugins,
		Errors:     per.Errors,
		Summary:    per.Summary,
		Scope:      scope,
	}
	if per.Summary.TotalRecords > 0 && tenant != "" {
		result.MatchedTenants = []string{tenant}
	}
	result.Incomplete = len(result.Errors) > 0 || scope.Covered < scope.Requested
	return result, nil
}

// SubjectExporterProvider is an optional interface a plugin can implement to
// expose its exporter rather than registering it from Start.
//
// The runtime type-asserts every active plugin to this interface after
// activation and registers what it hands back, so implementing it is enough on
// its own. Calling RegisterSubjectExporter as well is safe: Register ignores
// an exporter it already holds.
type SubjectExporterProvider interface {
	core.Plugin
	SubjectExporter() SubjectExporter
}

// ResetSubjectExporters replaces the global exporter registry with a fresh
// empty one. Use in tests to start with a clean slate before registering
// exporters.
func ResetSubjectExporters() {
	globalExporterRegistry = &SubjectExporterRegistry{}
}

// countRows totals every row-like slice anywhere in an exporter's payload,
// however deeply it nests them under headings.
func countRows(v any) int {
	switch t := v.(type) {
	case []map[string]any:
		return len(t)
	case []any:
		n := 0
		for _, e := range t {
			n += countRows(e)
		}
		if n == 0 {
			return len(t)
		}
		return n
	case map[string]any:
		n := 0
		for _, e := range t {
			n += countRows(e)
		}
		return n
	default:
		return 0
	}
}
