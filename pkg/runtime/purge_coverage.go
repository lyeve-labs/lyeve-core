package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// registerKernelPurgeHandlers registers the purge handlers for the engine's
// own tenant-scoped tables, which no plugin owns. They go in whichever plugins
// run, because the coverage check counts every table in the database. The
// tables of a plugin a build does not link are that build's to cover: it
// registers their handler before the engine boots. Registration is
// idempotent.
func registerKernelPurgeHandlers(logger *slog.Logger) {
	RegisterSysUserPurgeHandler(logger)
	RegisterCoreTablePurgeHandler(logger)
}

// checkPurgeCoverage lists every tenant_id-bearing table in the engine
// database and refuses the boot while any one of them has no registered purge
// handler. It runs after every compiled plugin has declared its purges
// (core.TenantPurger, asked whether or not the plugin starts) and every started
// plugin has registered its own (core.RegisterTenantPurgeHandler and
// core.RegisterCoveredTable).
//
// An uncovered table keeps a deleted tenant's rows, and nothing else would
// notice, so the engine does not start with one. A failed scan refuses the
// boot as well, because the check cannot run.
func checkPurgeCoverage(ctx context.Context, q core.Querier, dialect string, logger *slog.Logger) error {
	scoped, err := core.ListTenantScopedTables(ctx, q, dialect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: cannot list tenant-scoped tables for purge validation: %v\n", err)
		fmt.Fprintf(os.Stderr, "The purge registration validator must be able to query information_schema\n")
		fmt.Fprintf(os.Stderr, "to verify every tenant_id-bearing table has a registered purge handler.\n")
		fmt.Fprintf(os.Stderr, "Fix the database connection and restart.\n")
		return fmt.Errorf("purge validation: cannot list tenant-scoped tables: %w", err)
	}

	v := core.NewPurgeRegistrationValidator()
	missing := v.MissingTables(scoped)
	if len(missing) == 0 {
		logger.Info("purge registration validator passed",
			"covered_tables", len(core.CoveredTableSet()),
			"scoped_tables", len(scoped),
			"handlers", core.TenantPurgeHandlerCount())
		return nil
	}

	fmt.Fprintf(os.Stderr, "FATAL: %d tenant-scoped table(s) without a registered purge handler:\n", len(missing))
	for _, t := range missing {
		fmt.Fprintf(os.Stderr, "  - %s\n", t)
	}
	fmt.Fprintf(os.Stderr, "\nEvery table with a tenant_id column needs a purge path. The owning plugin\n")
	fmt.Fprintf(os.Stderr, "declares it through core.TenantPurger, which the engine asks of every\n")
	fmt.Fprintf(os.Stderr, "compiled plugin whether or not it starts.\n")
	fmt.Fprintf(os.Stderr, "Deleting a tenant would otherwise leave its rows in these tables, so the\n")
	fmt.Fprintf(os.Stderr, "engine refuses to start.\n")
	logger.Error("purge registration validator found uncovered tables",
		"uncovered", missing,
		"covered_tables", len(core.CoveredTableSet()),
		"scoped_tables", len(scoped),
		"handlers", core.TenantPurgeHandlerCount())
	return fmt.Errorf("purge validation: %d tenant-scoped table(s) lack registered purge handlers: %s", len(missing), strings.Join(missing, ", "))
}
