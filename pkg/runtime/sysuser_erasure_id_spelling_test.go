package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
)

// An account id spelled in upper case, in braces, with a urn:uuid: prefix or
// without hyphens parsed as the account's id for every plugin, which erased
// the rows kept under the account's address, while the account eraser
// compared the text, found no account, and the run answered complete. The
// account kept its address and stayed usable. The run now spells the id as
// the account records hold it before anything reads it.

// addressSeer records the addresses a run hands it.
type addressSeer struct{ seen []compliance.SubjectAddress }

func (*addressSeer) EraserName() string { return "mail" }

func (a *addressSeer) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	a.seen = compliance.SubjectAddresses(ctx, identifier)
	return 0, nil
}

func TestErasure_EverySpellingOfAnAccountIDErasesTheAccount_Postgres(t *testing.T) {
	testErasureEverySpellingOfAnAccountID(t, testdb.Postgres(t))
}

func TestErasure_EverySpellingOfAnAccountIDErasesTheAccount_MySQL(t *testing.T) {
	testErasureEverySpellingOfAnAccountID(t, testdb.MySQL(t))
}

func TestErasure_EverySpellingOfAnAccountIDErasesTheAccount_MSSQL(t *testing.T) {
	testErasureEverySpellingOfAnAccountID(t, testdb.MSSQL(t))
}

func testErasureEverySpellingOfAnAccountID(t *testing.T, pool db.DB) {
	t.Helper()
	ctx := context.Background()
	host := &revokingHost{Host: testhost.New(pool)}
	compliance.ResetHoldChecker()
	t.Cleanup(compliance.ResetHoldChecker)
	t.Cleanup(compliance.ResetSubjectErasers)

	for name, spell := range map[string]func(uuid.UUID) string{
		"upper case":     func(id uuid.UUID) string { return strings.ToUpper(id.String()) },
		"braces":         func(id uuid.UUID) string { return "{" + id.String() + "}" },
		"urn":            func(id uuid.UUID) string { return "urn:uuid:" + id.String() },
		"without hyphen": func(id uuid.UUID) string { return strings.ReplaceAll(id.String(), "-", "") },
	} {
		t.Run(name, func(t *testing.T) {
			compliance.ResetSubjectErasers()
			mail := &addressSeer{}
			compliance.RegisterSubjectEraser(newSysUserEraser(host))
			compliance.RegisterSubjectEraser(mail)
			id := uuid.New()
			email := "spelled-" + id.String()[:8] + "@example.com"
			_, err := pool.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash) VALUES ($1, $2, 'x')`, id, email)
			require.NoError(t, err)

			res, err := compliance.RunSubjectErasureWithHolds(ctx, spell(id))
			require.NoError(t, err)
			assert.Empty(t, res.Failed)
			require.Len(t, mail.seen, 1)
			assert.Equal(t, email, mail.seen[0].Address)

			row, err := pool.QueryRow(ctx, `SELECT anonymized, email FROM sys_users WHERE id = $1`, id)
			require.NoError(t, err)
			var anonymized bool
			var got string
			require.NoError(t, row.Scan(&anonymized, &got))
			assert.True(t, anonymized, "the account is erased")
			assert.NotEqual(t, email, got, "the account keeps no address")
		})
	}
}
