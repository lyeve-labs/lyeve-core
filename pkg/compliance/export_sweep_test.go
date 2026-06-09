package compliance

import (
	"context"
	"errors"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tenantScopedExporter answers from a per-tenant table, reading the tenant from
// the context exactly as the real exporters do.
type tenantScopedExporter struct {
	section string
	rows    map[string][]map[string]any // tenant -> rows
	failFor map[string]bool             // tenant -> return an error
}

func (e *tenantScopedExporter) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	tenant := core.TenantIDFromCtx(ctx)
	if e.failFor[tenant] {
		return nil, errors.New("boom")
	}
	rows, ok := e.rows[tenant]
	if !ok || len(rows) == 0 {
		return nil, nil
	}
	return map[string]any{e.section: rows}, nil
}

// passthroughScope stands in for AcquireTenantConn. The real one also rebinds
// the database connection, which is what makes MySQL and MSSQL work. The
// context value is the part a unit test can observe.
func passthroughScope(ctx context.Context, tenant string) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func resetRegistry(t *testing.T) {
	t.Helper()
	globalExporterRegistry = &SubjectExporterRegistry{}
}

func TestRunSubjectExportAcrossTenants_KeepsEachTenantsRowsApart(t *testing.T) {
	resetRegistry(t)
	RegisterSubjectExporter(&tenantScopedExporter{
		section: "example",
		rows: map[string][]map[string]any{
			"acme":   {{"to": "sam@example.com", "subject": "acme invoice"}},
			"globex": {{"to": "sam@example.com", "subject": "globex welcome"}},
		},
	})

	res, err := RunSubjectExportAcrossTenants(context.Background(),
		"sam@example.com", []string{"acme", "globex"}, passthroughScope)
	require.NoError(t, err)

	// The rows must not be merged. A flat bundle here would hand one
	// controller the other tenant's row for a person who may not be theirs.
	require.Len(t, res.Tenants, 2)
	assert.Equal(t, "acme invoice",
		res.Tenants["acme"].Plugins["example"].([]map[string]any)[0]["subject"])
	assert.Equal(t, "globex welcome",
		res.Tenants["globex"].Plugins["example"].([]map[string]any)[0]["subject"])
	assert.Empty(t, res.Plugins, "the flat bundle must stay empty for a sweep")
}

// The reason this option is dangerous, made visible instead of silent.
func TestRunSubjectExportAcrossTenants_FlagsAnIdentifierFoundInMoreThanOneTenant(t *testing.T) {
	resetRegistry(t)
	RegisterSubjectExporter(&tenantScopedExporter{
		section: "example",
		rows: map[string][]map[string]any{
			"acme":   {{"to": "sam@example.com"}},
			"globex": {{"to": "sam@example.com"}},
		},
	})

	res, err := RunSubjectExportAcrossTenants(context.Background(),
		"sam@example.com", []string{"acme", "globex"}, passthroughScope)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"acme", "globex"}, res.MatchedTenants,
		"both tenants matched, so the caller must be told this may be two people")
}

func TestRunSubjectExportAcrossTenants_DoesNotFlagASingleMatch(t *testing.T) {
	resetRegistry(t)
	RegisterSubjectExporter(&tenantScopedExporter{
		section: "example",
		rows:    map[string][]map[string]any{"acme": {{"to": "sam@example.com"}}},
	})

	res, err := RunSubjectExportAcrossTenants(context.Background(),
		"sam@example.com", []string{"acme", "globex"}, passthroughScope)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme"}, res.MatchedTenants)
	assert.False(t, res.Incomplete, "every tenant was reached and nothing failed")
}

// A bundle that is short must not say it is whole.
func TestRunSubjectExportAcrossTenants_ATenantItCouldNotReachMakesTheBundleIncomplete(t *testing.T) {
	resetRegistry(t)
	RegisterSubjectExporter(&tenantScopedExporter{
		section: "example",
		rows:    map[string][]map[string]any{"acme": {{"to": "sam@example.com"}}},
	})

	failing := func(ctx context.Context, tenant string) (context.Context, func(), error) {
		if tenant == "globex" {
			return nil, nil, errors.New("database unreachable")
		}
		return ctx, func() {}, nil
	}

	res, err := RunSubjectExportAcrossTenants(context.Background(),
		"sam@example.com", []string{"acme", "globex"}, failing)
	require.NoError(t, err)

	assert.True(t, res.Incomplete, "a tenant that was never reached is missing data")
	assert.Equal(t, 2, res.Scope.Requested)
	assert.Equal(t, 1, res.Scope.Covered)
	assert.Equal(t, []string{"acme"}, res.Scope.Tenants)
	require.Len(t, res.Errors, 1)
	assert.Contains(t, res.Errors[0].Error, "globex")
}

func TestRunSubjectExportAcrossTenants_AnExporterFailureMakesTheBundleIncomplete(t *testing.T) {
	resetRegistry(t)
	RegisterSubjectExporter(&tenantScopedExporter{
		section: "example",
		rows:    map[string][]map[string]any{"acme": {{"to": "sam@example.com"}}},
		failFor: map[string]bool{"globex": true},
	})

	res, err := RunSubjectExportAcrossTenants(context.Background(),
		"sam@example.com", []string{"acme", "globex"}, passthroughScope)
	require.NoError(t, err)
	assert.True(t, res.Incomplete)
	assert.Equal(t, 2, res.Scope.Covered, "the tenant was reached; its exporter failed")
}

// A single-tenant export must say which tenant it covered. Without this a
// bundle cannot be checked against the scope it was meant to have.
func TestRunSubjectExport_ReportsTheTenantItCovered(t *testing.T) {
	resetRegistry(t)
	RegisterSubjectExporter(&tenantScopedExporter{
		section: "example",
		rows:    map[string][]map[string]any{"acme": {{"to": "sam@example.com"}}},
	})

	ctx := core.WithTenantID(context.Background(), "acme")
	res, err := RunSubjectExport(ctx, "sam@example.com")
	require.NoError(t, err)
	assert.Equal(t, "tenant", res.Scope.Mode)
	assert.Equal(t, []string{"acme"}, res.Scope.Tenants)
	assert.Equal(t, 1, res.Summary.TotalRecords)
}
