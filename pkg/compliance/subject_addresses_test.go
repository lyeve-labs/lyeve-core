package compliance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// An account can hold addresses outside its own record, one for each identity
// linked to it, and the plugin that keeps the links erased them in the same
// run as everyone else. An eraser that matched rows by address reached only
// the account's own two, and a retry after a failure could no longer read the
// links. The erasure now resolves every address from the rows as they stand at
// the start of each run, and the links are erased only once every other
// eraser has succeeded.

// addressRecorder records the addresses each run hands it, and fails while
// broken is set.
type addressRecorder struct {
	broken bool
	runs   [][]compliance.SubjectAddress
}

func (r *addressRecorder) EraserName() string { return "mail" }

func (r *addressRecorder) EraseSubject(ctx context.Context, identifier string) (int64, error) {
	set := compliance.SubjectAddresses(ctx, identifier)
	r.runs = append(r.runs, set)
	if r.broken {
		return 0, errors.New("mail store is down")
	}
	return 0, nil
}

// inAcme is each address reached in the tenant acme.
func inAcme(addresses ...string) []compliance.SubjectAddress {
	out := make([]compliance.SubjectAddress, len(addresses))
	for i, a := range addresses {
		out[i] = compliance.SubjectAddress{Address: a, Tenant: "acme"}
	}
	return out
}

func TestRunSubjectErasure_ResolvesEveryAddressFromLiveRowsEachRun(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		compliance.ResetSubjectErasers()
		t.Cleanup(compliance.ResetSubjectErasers)
		compliance.ResetHoldChecker()
		t.Cleanup(compliance.ResetHoldChecker)

		links := "links_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
		_, err := q.Exec(ctx, `CREATE TABLE `+links+` (user_id VARCHAR(36) NOT NULL, address VARCHAR(255) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+links) })

		tag := uuid.NewString()[:8]
		id := uuid.New()
		email, provider := "own-"+tag+"@example.com", "idp-"+tag+"@example.com"
		_, err = q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, provider_email, tenant_id) VALUES ($1, $2, '', $3, 'acme')`, id, email, provider)
		require.NoError(t, err)
		_, err = q.Exec(ctx, `INSERT INTO `+links+` (user_id, address) VALUES ($1, $2), ($1, $3)`,
			id.String(), "gh-"+tag+"@example.com", "gl-"+tag+"@example.com")
		require.NoError(t, err)

		var linksErased []int
		run := 0
		compliance.RegisterSubjectEraser(plugintest.SubjectAccounts(host))
		compliance.RegisterSubjectEraser(compliance.NewDeclaredEraser("links", host, compliance.SubjectErasure{
			Tables: []string{links},
			Final:  true,
			Addresses: func(ctx context.Context, host core.Host, accountID string) ([]string, error) {
				rows, err := host.Querier(ctx).Query(ctx, `SELECT address FROM `+links+` WHERE user_id = $1`, accountID)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var out []string
				for rows.Next() {
					var a string
					if err := rows.Scan(&a); err != nil {
						return nil, err
					}
					out = append(out, a)
				}
				return out, rows.Err()
			},
			Erase: func(ctx context.Context, host core.Host, identifier string) (int64, error) {
				linksErased = append(linksErased, run)
				return 0, nil
			},
		}))
		mail := &addressRecorder{broken: true}
		compliance.RegisterSubjectEraser(mail)

		run = 1
		res, err := compliance.RunSubjectErasureWithHolds(ctx, id.String())
		require.Error(t, err)
		assert.Equal(t, []string{"mail", "links"}, res.Failed, "the links wait for every other eraser")
		assert.ElementsMatch(t, inAcme(email, provider, "gh-"+tag+"@example.com", "gl-"+tag+"@example.com"), mail.runs[0],
			"every address an account holds is reached in the account's own tenant")
		assert.Empty(t, linksErased)

		// Between the runs one link goes and the provider asserts another
		// address. The retry reads the rows as they are now.
		_, err = q.Exec(ctx, `DELETE FROM `+links+` WHERE address = $1`, "gl-"+tag+"@example.com")
		require.NoError(t, err)
		_, err = q.Exec(ctx, `UPDATE sys_users SET provider_email = $1 WHERE id = $2`, "idp2-"+tag+"@example.com", id)
		require.NoError(t, err)
		mail.broken = false

		run = 2
		res, err = compliance.RunSubjectErasureWithHolds(ctx, id.String())
		require.NoError(t, err)
		assert.Empty(t, res.Failed)
		assert.ElementsMatch(t, inAcme(email, "idp2-"+tag+"@example.com", "gh-"+tag+"@example.com"), mail.runs[1],
			"nothing is kept from the first run")
		assert.Equal(t, []int{2}, linksErased, "the links are erased on the run that completes")

		// A linked address alone names no account, so the set is the
		// address as typed, with its reach, and the plugin that keeps the
		// links matches its own rows by it.
		run = 3
		_, err = compliance.RunSubjectErasureWithHolds(ctx, "GH-"+tag+"@example.com")
		require.NoError(t, err)
		assert.Equal(t, []compliance.SubjectAddress{{Address: "GH-" + tag + "@example.com"}}, mail.runs[2])

		// An erasure by the account's own address keeps that address's reach
		// and reaches the rest in the account's tenant.
		_, err = compliance.RunSubjectErasureWithHolds(ctx, strings.ToUpper(email))
		require.NoError(t, err)
		assert.ElementsMatch(t, append([]compliance.SubjectAddress{{Address: strings.ToUpper(email)}},
			inAcme("idp2-"+tag+"@example.com", "gh-"+tag+"@example.com")...), mail.runs[3])
	})
}
