package runtime

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// keepPurgeRegistry puts the process-wide purge registry back as it was when
// the test ends, so what a test registers does not reach the next one.
func keepPurgeRegistry(t *testing.T) {
	t.Helper()
	tables := core.CoveredTableSet()
	handlers := core.TenantPurgeHandlers()
	t.Cleanup(func() {
		core.ResetCoveredTables()
		for table := range tables {
			core.RegisterCoveredTable(table)
		}
		core.ResetTenantPurgeHandlers()
		for _, h := range handlers {
			core.RegisterTenantPurgeHandler(h)
		}
	})
}

// coverEverything registers the engine's own purge handlers and then declares
// every other tenant-scoped table in the test database covered, standing in
// for the plugins a real boot would have started. It returns what the check
// needs to run against that database.
func coverEverything(t *testing.T) (context.Context, core.Querier, string, *slog.Logger) {
	t.Helper()
	keepPurgeRegistry(t)
	host := plugintest.Postgres(t)
	ctx := context.Background()
	q := host.Querier(ctx)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	registerKernelPurgeHandlers(logger)
	covered := core.CoveredTableSet()
	for _, table := range []string{"sys_users", "sys_plugin_config", "sys_device_logins"} {
		require.True(t, covered[table], "%s is covered by the engine whatever plugins run", table)
	}

	scoped, err := core.ListTenantScopedTables(ctx, q, host.Dialect())
	require.NoError(t, err)
	for _, table := range scoped {
		core.RegisterCoveredTable(table)
	}
	return ctx, q, host.Dialect(), logger
}

// A database whose every tenant-scoped table is covered boots.
func TestPurgeCoverage_BootsWhenEveryTableIsCovered(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	ctx, q, dialect, logger := coverEverything(t)
	require.NoError(t, checkPurgeCoverage(ctx, q, dialect, logger))
}

// The coverage check reads the database itself, so a table nobody covers is
// found with no plugin running at all, and the boot is refused with the table
// named.
func TestPurgeCoverage_RefusesBootOnATableNobodyCovers(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	ctx, q, dialect, logger := coverEverything(t)

	probe := "uncovered_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:10]
	_, err := q.Exec(ctx, `CREATE TABLE `+probe+` (id VARCHAR(36) PRIMARY KEY, tenant_id VARCHAR(255) NOT NULL)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+probe) })

	err = checkPurgeCoverage(ctx, q, dialect, logger)
	require.ErrorContains(t, err, "1 tenant-scoped table(s) lack registered purge handlers")
	require.ErrorContains(t, err, probe)

	core.RegisterCoveredTable(probe)
	require.NoError(t, checkPurgeCoverage(ctx, q, dialect, logger), "declaring the table covered lets the boot go on")
}
