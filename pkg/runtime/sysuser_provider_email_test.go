package runtime

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A subject request by address reaches the account a provider created in
// each tenant, which keeps the address in provider_email, as well as the
// account that holds it as its email. Export keeps to the tenant it runs
// for. Erasure is by address across the install and clears both provider
// columns.
func TestSysUserSubject_ProviderEmailIsExportedAndErased(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		accounts := host.(core.TenantAccountProvider).TenantAccounts()
		require.NotNil(t, accounts, "the test host serves the engine's store")
		address := "subject-" + uuid.NewString()[:8] + "@agency.example"

		var ids []uuid.UUID
		for _, tenant := range []string{"acme", "brand"} {
			acct, created, err := accounts.CreateTenantAccount(ctx, core.NewTenantAccount{
				TenantID: tenant, Address: address, PasswordHash: "hashed", ProviderSeen: true,
			})
			require.NoError(t, err)
			require.True(t, created)
			ids = append(ids, acct.ID)
		}
		_, err := host.Querier(ctx).Exec(ctx,
			`INSERT INTO sys_users (email, password_hash, tenant_id) VALUES ($1, $2, $3)`, address, "hashed", "third")
		require.NoError(t, err)

		for _, tenant := range []string{"acme", "brand", "third"} {
			rows := exportedRows(t, host, tenant, address)
			require.Len(t, rows, 1, "the account in %s", tenant)
			if tenant == "third" {
				assert.NotContains(t, rows[0], "provider_email")
				continue
			}
			assert.Equal(t, address, rows[0]["provider_email"])
			assert.Contains(t, rows[0], "provider_seen_at")
		}

		n, err := newSysUserEraser(host).EraseSubject(ctx, address)
		require.NoError(t, err)
		assert.Equal(t, int64(3), n)

		for _, tenant := range []string{"acme", "brand"} {
			_, err := accounts.FindProviderAccount(ctx, tenant, address)
			assert.ErrorIs(t, err, core.ErrNotFound, "the address is gone from %s", tenant)
		}
		for _, id := range ids {
			row, err := host.Querier(ctx).QueryRow(ctx,
				`SELECT COUNT(*) FROM sys_users WHERE id = $1 AND provider_email IS NULL AND provider_seen_at IS NULL`, id)
			require.NoError(t, err)
			var cleared int
			require.NoError(t, row.Scan(&cleared))
			assert.Equal(t, 1, cleared, "both provider columns are cleared")
		}

		for _, tenant := range []string{"acme", "brand", "third"} {
			assert.Nil(t, exportedRows(t, host, tenant, address), "nothing is held for the address in %s after erasure", tenant)
		}
	})
}
