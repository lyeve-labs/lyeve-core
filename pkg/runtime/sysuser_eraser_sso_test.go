package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// An account a SAML provider created has no password, so its hash is empty.
// Erasure by address anonymizes it like any other account, whether it keeps
// the address in provider_email or, from before tenant-local accounts, in
// email. A second erasure finds nothing left to erase.
func TestSysUserEraser_ErasesAccountsWithoutAPassword(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		accounts := host.(core.TenantAccountProvider).TenantAccounts()
		require.NotNil(t, accounts, "the test host serves the engine's store")
		address := "sso-" + uuid.NewString()[:8] + "@agency.example"

		local, created, err := accounts.CreateTenantAccount(ctx, core.NewTenantAccount{
			TenantID: "acme", Address: address, PasswordHash: "", ProviderSeen: true,
		})
		require.NoError(t, err)
		require.True(t, created)

		legacy := uuid.New()
		_, err = q.Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, tenant_id) VALUES ($1, $2, $3, $4)`,
			legacy, address, "", "brand")
		require.NoError(t, err)

		versions := map[uuid.UUID]int{}
		for _, id := range []uuid.UUID{local.ID, legacy} {
			row, err := q.QueryRow(ctx, `SELECT token_version FROM sys_users WHERE id = $1`, id)
			require.NoError(t, err)
			var v int
			require.NoError(t, row.Scan(&v))
			versions[id] = v
		}

		n, err := newSysUserEraser(host).EraseSubject(ctx, address)
		require.NoError(t, err)
		assert.Equal(t, int64(2), n, "both accounts without a password are erased")

		for id, before := range versions {
			row, err := q.QueryRow(ctx,
				`SELECT email, anonymized, token_version, provider_email, provider_seen_at FROM sys_users WHERE id = $1`, id)
			require.NoError(t, err)
			var (
				email         string
				anonymized    bool
				version       int
				providerEmail *string
				providerSeen  any
			)
			require.NoError(t, row.Scan(&email, &anonymized, &version, &providerEmail, &providerSeen))
			assert.True(t, strings.HasPrefix(email, "erased-"), "account %s keeps no address: %s", id, email)
			assert.True(t, anonymized, "account %s is marked anonymized", id)
			assert.Equal(t, before+1, version, "account %s has its tokens revoked", id)
			assert.Nil(t, providerEmail, "account %s keeps no provider address", id)
			assert.Nil(t, providerSeen, "account %s keeps no provider stamp", id)
		}

		again, err := newSysUserEraser(host).EraseSubject(ctx, local.ID.String())
		require.NoError(t, err)
		assert.Equal(t, int64(0), again, "an erased account is not erased twice")
	})
}
