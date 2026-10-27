package db_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The same address, spelled with a precomposed é and with e and a combining
// accent. The local part is unique per test so the shared fixtures stay apart.
func addressForms(t *testing.T) (composed, decomposed string) {
	t.Helper()
	local := "josé-" + uuid.NewString()[:8]
	composed = local + "@example.com"
	return composed, strings.Replace(composed, "é", "é", 1)
}

// An account created, signed in or provisioned with either spelling stores
// and finds the composed one, so one address makes one account.
func TestUserStore_AccountAddressesAreStoredInOneForm(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		composed, decomposed := addressForms(t)

		u, err := users.Create(ctx, strings.ToUpper(decomposed), tenantLocalHash, nil, "acme")
		require.NoError(t, err)
		assert.Equal(t, composed, u.Email)
		got, err := users.GetByEmail(ctx, composed)
		require.NoError(t, err, "a sign-in with the composed spelling finds the account")
		assert.Equal(t, u.ID, got.ID)
		_, err = users.Create(ctx, composed, tenantLocalHash, nil, "acme")
		assert.Error(t, err, "the composed spelling is the address the account holds")

		other, _ := addressForms(t)
		otherDecomposed := strings.Replace(other, "é", "é", 1)
		a, created, err := users.CreateTenantLocal(ctx, "acme", otherDecomposed, "", nil, true)
		require.NoError(t, err)
		require.True(t, created)
		assert.Equal(t, other, a.ProviderEmail)
		b, created, err := users.CreateTenantLocal(ctx, "acme", other, "", nil, true)
		require.NoError(t, err)
		assert.False(t, created, "the second spelling finds the first account")
		assert.Equal(t, a.ID, b.ID)
	})
}

// A value that is not one bare address is refused before anything is stored.
func TestUserStore_AccountAddressIsCheckedAsAnAddress(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		for _, bad := range []string{"not-an-address", "Person <person@example.com>", "person@example.com, other@example.com", "@example.com"} {
			_, err := users.Create(ctx, bad, tenantLocalHash, nil, "acme")
			assert.ErrorIs(t, err, domain.ErrValidation, "create %q", bad)
			_, _, err = users.CreateTenantLocal(ctx, "acme", bad, "", nil, true)
			assert.ErrorIs(t, err, domain.ErrValidation, "tenant-local %q", bad)
			_, err = users.CreateFirstAdmin(ctx, bad, tenantLocalHash, "")
			assert.ErrorIs(t, err, domain.ErrValidation, "first admin %q", bad)
		}
		n, err := users.Count(ctx)
		require.NoError(t, err)
		assert.Zero(t, n)
	})
}

// seedAccount stores an account holding value in column exactly as given,
// created at created, the way a row written before the normalized form looks.
func seedAccount(t *testing.T, pool db.DB, column, tenant, value string, created time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	users := db.NewUserStore(pool)
	placeholder := "seed-" + uuid.NewString()[:8] + "@example.com"
	var id uuid.UUID
	if column == "email" {
		u, err := users.Create(ctx, placeholder, tenantLocalHash, nil, tenant)
		require.NoError(t, err)
		id = u.ID
	} else {
		u, _, err := users.CreateTenantLocal(ctx, tenant, placeholder, "", nil, true)
		require.NoError(t, err)
		id = u.ID
	}
	_, err := pool.Exec(ctx, `UPDATE sys_users SET `+column+` = $1, created_at = $2 WHERE id = $3`, value, created, id)
	if err != nil {
		require.True(t, core.IsDuplicateKey(err), "only the unique key may refuse a seed: %v", err)
		return uuid.Nil
	}
	return id
}

func storedAddress(t *testing.T, pool db.DB, column string, id uuid.UUID) string {
	t.Helper()
	row, err := pool.QueryRow(context.Background(), `SELECT `+column+` FROM sys_users WHERE id = $1`, id)
	require.NoError(t, err)
	var v string
	require.NoError(t, row.Scan(&v))
	return v
}

func normalizeAccountEmails(t *testing.T, pool db.DB) (db.AccountEmailFormReport, string) {
	t.Helper()
	var log bytes.Buffer
	report, err := db.NormalizeAccountEmails(context.Background(), pool, slog.New(slog.NewJSONHandler(&log, nil)))
	require.NoError(t, err)
	require.False(t, report.Skipped)
	return report, log.String()
}

// Rows written before the form are rewritten into it, in one tenant and in
// two, and a second run rewrites nothing.
func TestNormalizeAccountEmails_RewritesEachAccountIntoTheForm(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		users := db.NewUserStore(pool)
		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		composed, decomposed := addressForms(t)
		local, localDecomposed := addressForms(t)

		own := seedAccount(t, pool, "email", "acme", decomposed, base)
		acme := seedAccount(t, pool, "provider_email", "acme", localDecomposed, base)
		brand := seedAccount(t, pool, "provider_email", "brand", localDecomposed, base.Add(time.Minute))
		untouched := seedAccount(t, pool, "email", "acme", "plain-"+uuid.NewString()[:8]+"@example.com", base)
		for _, id := range []uuid.UUID{own, acme, brand, untouched} {
			require.NotEqual(t, uuid.Nil, id)
		}
		before := storedAddress(t, pool, "email", untouched)

		report, log := normalizeAccountEmails(t, pool)
		assert.Equal(t, 3, report.Rewritten)
		assert.Empty(t, report.Collisions, "one address in two tenants is two tenant-local accounts")
		assert.Equal(t, composed, storedAddress(t, pool, "email", own))
		assert.Equal(t, local, storedAddress(t, pool, "provider_email", acme))
		assert.Equal(t, local, storedAddress(t, pool, "provider_email", brand))
		assert.Equal(t, before, storedAddress(t, pool, "email", untouched))
		assert.NotContains(t, log, "example.com", "the report names no address")

		got, err := users.GetByEmail(ctx, decomposed)
		require.NoError(t, err, "a sign-in with either spelling finds the rewritten account")
		assert.Equal(t, own, got.ID)
		got, err = users.GetByTenantProviderEmail(ctx, "brand", local)
		require.NoError(t, err)
		assert.Equal(t, brand, got.ID)

		again, _ := normalizeAccountEmails(t, pool)
		assert.Zero(t, again.Rewritten)
		assert.Empty(t, again.Collisions)
	})
}

// Accounts that hold one address in different forms keep the address on the
// oldest, or on the one that already holds the form, and the others keep their
// bytes and are reported by id on every run. A database whose unique key
// already compares the forms as one refuses the pair, and has nothing to
// report.
func TestNormalizeAccountEmails_ReportsAccountsThatCollide(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

		// Both rows out of the form: the oldest takes it. The two spell
		// the second accent differently.
		pair := "zoë-josé-" + uuid.NewString()[:8] + "@example.com"
		mixed := strings.Replace(pair, "ë", "ë", 1)
		form := core.NormalizeEmail(pair)
		newer := seedAccount(t, pool, "email", "brand", mixed, base.Add(time.Minute))
		oldest := seedAccount(t, pool, "email", "acme", pair, base)

		// One row already in the form: it keeps the address though the
		// other is older.
		composed, decomposed := addressForms(t)
		holder, _, err := db.NewUserStore(pool).CreateTenantLocal(ctx, "acme", composed, "", nil, true)
		require.NoError(t, err)
		older := seedAccount(t, pool, "provider_email", "acme", decomposed, base)

		if oldest == uuid.Nil || older == uuid.Nil {
			assert.Equal(t, uuid.Nil, oldest, "the unique key refuses both pairs or neither")
			assert.Equal(t, uuid.Nil, older, "the unique key refuses both pairs or neither")
			report, _ := normalizeAccountEmails(t, pool)
			assert.Empty(t, report.Collisions)
			assert.Equal(t, form, storedAddress(t, pool, "email", newer))
			return
		}

		report, log := normalizeAccountEmails(t, pool)
		assert.Equal(t, 1, report.Rewritten)
		assert.Equal(t, form, storedAddress(t, pool, "email", oldest))
		assert.Equal(t, mixed, storedAddress(t, pool, "email", newer), "the newer account keeps its bytes")
		assert.Equal(t, composed, storedAddress(t, pool, "provider_email", holder.ID))
		assert.Equal(t, decomposed, storedAddress(t, pool, "provider_email", older), "the form stays with the account that held it")
		assert.ElementsMatch(t, []db.AccountEmailCollision{
			{Column: "email", HolderID: oldest, AccountIDs: []uuid.UUID{newer}, TenantIDs: []string{"acme", "brand"}},
			{Column: "provider_email", HolderID: holder.ID, AccountIDs: []uuid.UUID{older}, TenantIDs: []string{"acme"}},
		}, report.Collisions)
		for _, id := range []uuid.UUID{oldest, newer, holder.ID, older} {
			assert.Contains(t, log, id.String())
		}
		assert.NotContains(t, log, "example.com", "the report names no address")

		again, _ := normalizeAccountEmails(t, pool)
		assert.Zero(t, again.Rewritten)
		assert.ElementsMatch(t, report.Collisions, again.Collisions, "every run reports what an operator has not decided")
	})
}
