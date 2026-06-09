package compliance

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// TenantScopeFunc binds ctx to a single tenant and returns a cleanup the caller
// must run. It matches core.TenancyConnProvider.AcquireTenantConn, which is the
// engine's own scoping path: on MySQL and MSSQL the tenant lives in a separate
// database, so scoping by setting a context value alone leaves the query
// pointed at the wrong one.
type TenantScopeFunc func(ctx context.Context, tenant string) (context.Context, func(), error)

// TenantLister enumerates every tenant slug on the install. It is supplied by
// the caller rather than read here, so this package cannot widen its own scope.
type TenantLister func(ctx context.Context) ([]string, error)

// ExportScope records what a bundle actually covered. It is always populated,
// because the failure this exists to prevent is a partial bundle that reads as
// a whole one.
type ExportScope struct {
	// Mode is "tenant" for a single-tenant export or "all_tenants" for a sweep.
	Mode string `json:"mode"`

	// Tenants lists the tenants the export actually reached.
	Tenants []string `json:"tenants"`

	// Requested is how many tenants the sweep set out to cover, and Covered is
	// how many it reached. They differ when a tenant could not be scoped, which
	// makes the bundle incomplete.
	Requested int `json:"tenants_requested"`
	Covered   int `json:"tenants_covered"`
}

// TenantExport is one tenant's slice of a swept bundle.
type TenantExport struct {
	Tenant  string         `json:"tenant"`
	Plugins map[string]any `json:"plugins"`
	Summary ExportSummary  `json:"summary"`
	Errors  []ExportError  `json:"errors"`
}

// RunSubjectExportAcrossTenants runs the full exporter set once per tenant and
// returns the results keyed by tenant.
//
// The results are deliberately NOT merged into one flat bundle. An identifier
// like an email address is not unique across tenants: the same address can
// belong to different people in two tenants, so merging them produces a bundle
// that hands one controller another person's records, which is a breach rather
// than a complete answer. Keyed by tenant, a controller can see which tenant
// each row came from and decide what belongs in their response.
//
// MatchedTenants on the result names every tenant that returned data. More than
// one is the case a caller has to look at before releasing the bundle.
//
// The caller is responsible for the super_admin gate and for deciding which
// tenants to pass. This function does not enumerate them, so it cannot be
// tricked into widening its own scope.
func RunSubjectExportAcrossTenants(
	ctx context.Context,
	identifier string,
	tenants []string,
	scope TenantScopeFunc,
) (*SubjectExportResult, error) {
	result := &SubjectExportResult{
		Identifier: identifier,
		Plugins:    map[string]any{},
		Errors:     []ExportError{},
		Tenants:    map[string]*TenantExport{},
		Scope: ExportScope{
			Mode:      "all_tenants",
			Tenants:   []string{},
			Requested: len(tenants),
		},
	}

	for _, tenant := range tenants {
		tctx := ctx
		cleanup := func() {}
		if scope != nil {
			bound, done, err := scope(ctx, tenant)
			if err != nil {
				// A tenant that cannot be scoped is data the bundle is missing,
				// not a tenant with no data. Recorded so Incomplete is true and
				// the gap is nameable.
				slog.ErrorContext(ctx, "subject export: tenant could not be scoped",
					"tenant", tenant, "err", err)
				result.Errors = append(result.Errors, ExportError{
					Index: -1,
					Error: "tenant could not be scoped: " + tenant,
				})
				continue
			}
			tctx, cleanup = bound, done
		}
		tctx = core.WithTenantID(tctx, tenant)

		per := runExportersInto(tctx, identifier)
		cleanup()

		per.Tenant = tenant
		result.Tenants[tenant] = per
		result.Scope.Tenants = append(result.Scope.Tenants, tenant)
		result.Scope.Covered++

		result.Summary.TotalRecords += per.Summary.TotalRecords
		result.Summary.PluginsQueried += per.Summary.PluginsQueried
		result.Summary.PluginsWithData += per.Summary.PluginsWithData
		result.Errors = append(result.Errors, per.Errors...)

		if per.Summary.TotalRecords > 0 {
			result.MatchedTenants = append(result.MatchedTenants, tenant)
		}
	}

	// Incomplete covers both ways a bundle can be short: an exporter failed, or
	// a tenant was never reached.
	//
	// The coverage half adds nothing while every skip above also records an
	// error, and no test can reach it on its own. It is kept as the
	// invariant rather than the mechanism: a later skip path that forgets to
	// record an error would otherwise report a short sweep as whole.
	result.Incomplete = len(result.Errors) > 0 || result.Scope.Covered < result.Scope.Requested

	return result, nil
}

// runExportersInto runs every registered exporter against one already-scoped
// context. It is the per-tenant half of the sweep and the body RunSubjectExport
// uses for the single-tenant case, so the two cannot drift.
func runExportersInto(ctx context.Context, identifier string) *TenantExport {
	exporters := globalExporterRegistry.Exporters()
	out := &TenantExport{
		Plugins: map[string]any{},
		Errors:  []ExportError{},
	}

	for i, exporter := range exporters {
		data, err := exporter.ExportSubject(ctx, identifier)
		if err != nil {
			// The identifier is the personal data the request is about, and a
			// stored log line outlives the bundle, so the line names the
			// subject by the digest the erasure lines use.
			slog.ErrorContext(ctx, "subject_export_failed",
				"exporter_index", i,
				"subject_ref", subjectRef(identifier),
				"err", err,
			)
			out.Errors = append(out.Errors, ExportError{
				Index: i,
				Error: "export failed for plugin",
			})
			continue
		}
		out.Summary.PluginsQueried++
		if data == nil {
			continue
		}
		out.Summary.PluginsWithData++
		out.Summary.TotalRecords += countRows(data)
		for key, val := range data {
			if _, taken := out.Plugins[key]; taken {
				qualified := fmt.Sprintf("%s_%d", key, i)
				slog.WarnContext(ctx, "subject export section name collision",
					"section", key, "kept_as", qualified, "exporter_index", i)
				out.Plugins[qualified] = val
				continue
			}
			out.Plugins[key] = val
		}
	}
	return out
}
