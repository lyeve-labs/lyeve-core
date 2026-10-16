package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// seedSubjectAccounts stores address as the email of an account in acme and
// as the provider address of an account in brand, the two columns a subject
// request compares, and returns both ids.
func seedSubjectAccounts(t *testing.T, host core.Host, tag, address string) (byEmail, byProvider uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	q := host.Querier(ctx)
	byEmail, byProvider = uuid.New(), uuid.New()
	_, err := q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, tenant_id) VALUES ($1, $2, $3, $4)`,
		byEmail, address, "hashed", "acme")
	require.NoError(t, err)
	_, err = q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, tenant_id, provider_email) VALUES ($1, $2, $3, $4, $5)`,
		byProvider, "local-"+tag+"@brand.invalid", "hashed", "brand", address)
	require.NoError(t, err)
	return byEmail, byProvider
}

// storedAccount reads an account's addresses back as stored. The comparison
// happens in Go, because a collation that folds accents would answer an
// equality test in SQL.
func storedAccount(t *testing.T, host core.Host, id uuid.UUID) (email, providerEmail string) {
	t.Helper()
	ctx := context.Background()
	row, err := host.Querier(ctx).QueryRow(ctx, `SELECT email, provider_email FROM sys_users WHERE id = $1`, id)
	require.NoError(t, err)
	var provider sql.NullString
	require.NoError(t, row.Scan(&email, &provider))
	return email, provider.String
}

// Two addresses that differ only in an accent, or in a letter a collation
// expands, belong to two people. MySQL's collations ignore accents and read ß
// as ss, and SQL Server's default collation reads ß as ss, so a request for an
// address that holds no account reached the account of its variant, and the
// erasure anonymized another person.
func TestSysUserSubject_AccentVariantIsAnotherPerson(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		for _, pair := range []struct{ name, stored, requested string }{
			{"accent", "josé-%s@x.example", "jose-%s@x.example"},
			{"sharp s", "straße-%s@x.example", "strasse-%s@x.example"},
		} {
			t.Run(pair.name, func(t *testing.T) {
				tag := uuid.NewString()[:8]
				stored, requested := fmt.Sprintf(pair.stored, tag), fmt.Sprintf(pair.requested, tag)
				byEmail, byProvider := seedSubjectAccounts(t, host, tag, stored)

				for _, tenant := range []string{"acme", "brand"} {
					assert.Nil(t, exportedRows(t, host, tenant, requested),
						"the export of %s in %s on %s", requested, tenant, host.Dialect())
				}

				n, err := newSysUserEraser(host).EraseSubject(ctx, requested)
				require.NoError(t, err)
				assert.Zero(t, n, "rows erased for %s on %s", requested, host.Dialect())
				email, _ := storedAccount(t, host, byEmail)
				assert.Equal(t, stored, email, "the account of %s survives on %s", stored, host.Dialect())
				_, provider := storedAccount(t, host, byProvider)
				assert.Equal(t, stored, provider, "the provider address %s survives on %s", stored, host.Dialect())
			})
		}
	})
}

// The subject's own account can hold the address in another form than the
// request: decomposed, with capitals, or with a trailing space. Each still
// names the subject, so the export lists the account and the erasure reaches
// it.
func TestSysUserSubject_OwnAccountInAnotherFormIsReached(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		for _, probe := range []struct{ name, stored, requested string }{
			{"decomposed accent", "jose\u0301-%s@x.example", "jos\u00e9-%s@x.example"},
			{"capitals", "Dana-%s@X.Example", "dana-%s@x.example"},
			{"trailing space", "erin-%s@x.example ", "erin-%s@x.example"},
		} {
			t.Run(probe.name, func(t *testing.T) {
				tag := uuid.NewString()[:8]
				stored, requested := fmt.Sprintf(probe.stored, tag), fmt.Sprintf(probe.requested, tag)
				byEmail, byProvider := seedSubjectAccounts(t, host, tag, stored)

				for _, tenant := range []string{"acme", "brand"} {
					assert.Len(t, exportedRows(t, host, tenant, requested), 1,
						"the export of %s in %s on %s", requested, tenant, host.Dialect())
				}

				n, err := newSysUserEraser(host).EraseSubject(ctx, requested)
				require.NoError(t, err)
				assert.Equal(t, int64(2), n, "rows erased for %s on %s", requested, host.Dialect())
				email, _ := storedAccount(t, host, byEmail)
				assert.NotEqual(t, stored, email, "the account of %s is erased on %s", stored, host.Dialect())
				_, provider := storedAccount(t, host, byProvider)
				assert.Empty(t, provider, "the provider address %s is erased on %s", stored, host.Dialect())
			})
		}
	})
}
