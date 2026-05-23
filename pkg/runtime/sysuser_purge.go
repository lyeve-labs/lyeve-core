// Package runtime registration of the sys_users purge handler for tenant deletion.
// Without this registration the boot-time purge validator reports sys_users as
// uncovered: every tenant_id-bearing table needs a registered handler.
package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RegisterSysUserPurgeHandler registers a TenantPurgeHandler for the
// sys_users table and declares it as a covered table via RegisterCoveredTable.
// Idempotent: safe to call multiple times.
//
// The handler DELETEs every sys_users row whose tenant_id matches the tenant
// slug being deleted. Deletion (rather than anonymization) is correct here:
// the entire tenant's data is being purged atomically inside the delete
// transaction, so there is no reason to leave anonymized user stubs behind.
// Callers with DSAR obligations use the separate SubjectEraser (sysuser_eraser.go)
// which anonymizes individual records on a per-subject basis.
func RegisterSysUserPurgeHandler(logger *slog.Logger) {
	covered := core.CoveredTableSet()
	if covered["sys_users"] {
		logger.Warn("sys_users purge handler already registered - skipping duplicate registration")
		return
	}

	core.RegisterTenantPurgeHandler(deleteSysUsersRows)
	core.RegisterCoveredTable("sys_users")
	logger.Info("sys_users purge handler registered - DELETEs sys_users rows on tenant deletion")
}

// deleteSysUsersRows deletes every sys_users row belonging to the given
// tenant slug. Called inside the tenant delete transaction.
func deleteSysUsersRows(ctx context.Context, q core.Querier, slug string) error {
	tag, err := q.Exec(ctx, `DELETE FROM sys_users WHERE tenant_id = $1`, slug)
	if err != nil {
		return fmt.Errorf("sys_users purge: %w", err)
	}
	slog.DebugContext(ctx, "sys_users purge rows affected",
		"slug", slug, "rows", tag.RowsAffected)
	return nil
}
