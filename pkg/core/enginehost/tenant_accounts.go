package enginehost

import (
	"context"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

var (
	_ core.ProviderWindowProvider = (*engineHost)(nil)
	_ core.TenantAccountProvider  = (*engineHost)(nil)
)

// ProviderWindowDays implements core.ProviderWindowProvider through the same
// reader the engine's password login uses.
func (h *engineHost) ProviderWindowDays(ctx context.Context, tenantID string) (int, error) {
	return config.NewProviderWindows(h.pool).ProviderWindowDays(ctx, tenantID)
}

// ProviderWindowAdmitsAccount implements core.ProviderWindowProvider.
func (h *engineHost) ProviderWindowAdmitsAccount(ctx context.Context, tenantID string, seenAt *time.Time) (bool, error) {
	return config.NewProviderWindows(h.pool).ProviderWindowAdmitsAccount(ctx, tenantID, seenAt)
}

// TenantAccounts implements core.TenantAccountProvider.
func (h *engineHost) TenantAccounts() core.TenantAccountStore {
	return db.NewTenantAccountStore(h.pool)
}
