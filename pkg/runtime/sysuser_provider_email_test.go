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

// A subject request by address reaches the accounts a provider created in
// every tenant, which keep the address in provider_email, as well as the
// account that holds it as its email. Erasure clears both provider columns.
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

		got, err := newSysUserExporter(host).ExportSubject(ctx, address)
		require.NoError(t, err)
		rows, ok := got["sys_users"].([]map[string]any)
		require.True(t, ok, "%#v", got)
		require.Len(t, rows, 3, "every tenant's account for the address")
		local := 0
		for _, row := range rows {
			if row["provider_email"] == address {
				local++
				assert.Contains(t, row, "provider_seen_at")
			}
		}
		assert.Equal(t, 2, local)

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

		after, err := newSysUserExporter(host).ExportSubject(ctx, address)
		require.NoError(t, err)
		assert.Nil(t, after, "nothing is held for the address after erasure")
	})
}
