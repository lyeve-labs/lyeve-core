package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// distinctAddresses are pairs of different addresses that a database
// collation may compare as equal.
var distinctAddresses = []struct{ name, stored, asked string }{
	{"accent", "josé", "jose"},
	{"sharp s", "straße", "strasse"},
	{"ligature", "ﬁ", "fi"},
}

// A lookup by address finds only the account that holds that address, on
// every dialect, for the install-wide, tenant and tenant-local reads.
func TestUserStore_AddressLookupFindsOnlyTheHolder(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		for _, pair := range distinctAddresses {
			t.Run(pair.name, func(t *testing.T) {
				suffix := "-" + uuid.NewString()[:8] + "@example.com"
				stored, asked := pair.stored+suffix, pair.asked+suffix

				own, err := users.Create(ctx, stored, tenantLocalHash, nil, "acme")
				require.NoError(t, err)
				got, err := users.GetByEmail(ctx, stored)
				require.NoError(t, err)
				assert.Equal(t, own.ID, got.ID)
				_, err = users.GetByEmail(ctx, asked)
				assert.ErrorIs(t, err, domain.ErrNotFound, "install-wide")
				_, err = users.GetByTenantAddress(ctx, "acme", asked)
				assert.ErrorIs(t, err, domain.ErrNotFound, "tenant")

				local, created, err := users.CreateTenantLocal(ctx, "brand", stored, "", nil, true)
				require.NoError(t, err)
				require.True(t, created)
				got, err = users.GetByTenantProviderEmail(ctx, "brand", stored)
				require.NoError(t, err)
				assert.Equal(t, local.ID, got.ID)
				_, err = users.GetByTenantProviderEmail(ctx, "brand", asked)
				assert.ErrorIs(t, err, domain.ErrNotFound, "tenant-local")
				_, err = users.GetByTenantAddress(ctx, "brand", asked)
				assert.ErrorIs(t, err, domain.ErrNotFound, "tenant, tenant-local first")

				other, _, err := users.CreateTenantLocal(ctx, "brand", asked, "", nil, true)
				if err == nil {
					assert.NotEqual(t, local.ID, other.ID, "a create for another address returned this account")
				}
			})
		}
	})
}
