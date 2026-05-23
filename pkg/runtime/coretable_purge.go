// Package runtime registration of purge handlers for the tenant-scoped tables
// core owns itself. A plugin registers its own. These two are created by core's
// migrations, so nothing else would.
package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// coreOwnedTenantTables are the tenant_id-bearing tables created by core's own
// migrations rather than by a plugin.
//
// sys_plugin_config holds operator-set plugin configuration. It is keyed by
// tenant_id, so its rows go with the tenant, or the next owner of the slug
// would read a deleted tenant's configuration.
//
// sys_device_logins holds device sign-ins. A pending one names no tenant and
// expires on its own. An approved one is bound to the tenant it signs in to
// and goes with it.
var coreOwnedTenantTables = []string{
	"sys_plugin_config",
	"sys_device_logins",
}

// RegisterCoreTablePurgeHandler registers a TenantPurgeHandler for the
// tenant-scoped tables core creates for itself, and declares each one covered.
// Idempotent: a table already declared by someone else is left to them.
func RegisterCoreTablePurgeHandler(logger *slog.Logger) {
	covered := core.CoveredTableSet()
	var mine []string
	for _, t := range coreOwnedTenantTables {
		if covered[t] {
			logger.Warn("core table purge handler already registered, skipping duplicate", "table", t)
			continue
		}
		mine = append(mine, t)
	}
	if len(mine) == 0 {
		return
	}

	core.RegisterTenantPurgeHandler(func(ctx context.Context, q core.Querier, slug string) error {
		return deleteCoreTableRows(ctx, q, slug, mine)
	})
	for _, t := range mine {
		core.RegisterCoveredTable(t)
	}
	logger.Info("core table purge handler registered", "tables", mine)
}

// deleteCoreTableRows deletes every row belonging to the given tenant slug from
// each table. Runs inside the tenant delete transaction.
func deleteCoreTableRows(ctx context.Context, q core.Querier, slug string, tables []string) error {
	for _, t := range tables {
		// t comes from coreOwnedTenantTables, a package-level literal, so it is
		// not attacker-reachable. The slug is still bound.
		tag, err := q.Exec(ctx, `DELETE FROM `+t+` WHERE tenant_id = $1`, slug)
		if err != nil {
			return fmt.Errorf("%s purge: %w", t, err)
		}
		slog.DebugContext(ctx, "core table purge rows affected",
			"table", t, "slug", slug, "rows", tag.RowsAffected)
	}
	return nil
}
