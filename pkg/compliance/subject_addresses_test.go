package compliance_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

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

// An identity provider can assert any text as an account's address, and
// nothing checks it is one. An account whose provider asserted another
// account's id handed that id to every eraser and exporter, which then reached
// the rows naming the other account. Only email addresses are resolved now.
func TestRunSubjectErasure_ResolvesOnlyEmailAddresses(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		compliance.ResetSubjectErasers()
		t.Cleanup(compliance.ResetSubjectErasers)
		compliance.ResetHoldChecker()
		t.Cleanup(compliance.ResetHoldChecker)

		tag := uuid.NewString()[:8]
		other, id := uuid.New(), uuid.New()
		email := "own-" + tag + "@example.com"
		_, err := q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, tenant_id) VALUES ($1, $2, '', 'acme')`, other, "other-"+tag+"@example.com")
		require.NoError(t, err)
		_, err = q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, provider_email, tenant_id) VALUES ($1, $2, '', $3, 'acme')`, id, email, other.String())
		require.NoError(t, err)

		compliance.RegisterSubjectEraser(plugintest.SubjectAccounts(host))
		compliance.RegisterSubjectEraser(compliance.NewDeclaredEraser("links", host, compliance.SubjectErasure{
			Tables: []string{"sys_users"},
			Addresses: func(ctx context.Context, host core.Host, accountID string) ([]string, error) {
				return []string{"gh-" + tag + "@example.com", "Jane " + tag, "Jane <jane-" + tag + "@example.com>"}, nil
			},
			Erase: func(ctx context.Context, host core.Host, identifier string) (int64, error) { return 0, nil },
		}))
		mail := &addressRecorder{}
		compliance.RegisterSubjectEraser(mail)

		var buf bytes.Buffer
		restore := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(restore) })

		_, err = compliance.RunSubjectErasureWithHolds(ctx, id.String())
		require.NoError(t, err)
		require.Len(t, mail.runs, 1)
		assert.ElementsMatch(t, inAcme(email, "gh-"+tag+"@example.com"), mail.runs[0],
			"the account's own address and a linked one resolve, the other account's id and the text that is no address do not")

		out := buf.String()
		assert.Equal(t, 1, strings.Count(out, "subject_addresses_dropped"), "one line per run")
		assert.Contains(t, out, `"count":3`)
		for _, dropped := range []string{other.String(), "Jane " + tag, "jane-" + tag} {
			assert.NotContains(t, out, dropped, "the log names no address")
		}
	})
}

// Account uniqueness compares bytes while SameAddress compares an address in
// Unicode normalization form C, so two accounts in one tenant can hold
// josé@ composed and decomposed, and an identity provider can link one
// account to another's address. Each account's request then reached the
// other's rows under that address. An address read from an account now
// reaches rows only while no other account in its tenant holds it.
func TestResolveSubjectAddresses_AnAddressAnotherAccountHoldsReachesNeither(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)

		links := "links_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
		text := "NVARCHAR(255)"
		if host.Dialect() == "postgres" {
			text = "VARCHAR(255)"
		}
		_, err := q.Exec(ctx, `CREATE TABLE `+links+` (user_id VARCHAR(36) NOT NULL, tenant_id VARCHAR(64) NOT NULL, address `+text+` NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+links) })

		tag := uuid.NewString()[:8]
		at := func(local string) string { return local + "-" + tag + "@example.com" }
		composed := norm.NFC.String(at("josé"))
		decomposed := norm.NFD.String(composed)
		require.NotEqual(t, composed, decomposed)

		account := func(tenant, email, provider string) uuid.UUID {
			id := uuid.New()
			_, err := q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, provider_email, tenant_id) VALUES ($1, $2, '', $3, $4)`, id, email, provider, tenant)
			require.NoError(t, err)
			return id
		}
		// PostgreSQL's unique key on email compares bytes, so b holds the
		// decomposed form as its email there. The other two fold the forms
		// in that key, and b holds it as its provider's address.
		a := account("acme", composed, at("a-idp"))
		bOwn := at("b")
		b := account("acme", bOwn, decomposed)
		if host.Dialect() == "postgres" {
			bOwn = at("b-idp")
			_, err := q.Exec(ctx, `UPDATE sys_users SET email = $1, provider_email = $2 WHERE id = $3`, decomposed, bOwn, b)
			require.NoError(t, err)
		}
		c := account("acme", at("c"), at("c-idp"))
		d := account("acme", at("d"), at("d-idp"))
		e := account("acme", at("e"), at("e-idp"))
		g := account("globex", at("g"), at("e"))
		_, err = q.Exec(ctx, `INSERT INTO `+links+` (user_id, tenant_id, address) VALUES ($1, 'acme', $2)`, c.String(), strings.ToUpper(at("d")))
		require.NoError(t, err)

		scan := func(rows core.Rows, err error) ([]compliance.SubjectAccount, error) {
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []compliance.SubjectAccount
			for rows.Next() {
				var held compliance.SubjectAccount
				var address string
				if err := rows.Scan(&held.ID, &held.Tenant, &address); err != nil {
					return nil, err
				}
				held.Addresses = []string{address}
				out = append(out, held)
			}
			return out, rows.Err()
		}
		erasers := []compliance.SubjectEraser{plugintest.SubjectAccounts(host), compliance.NewDeclaredEraser("links", host, compliance.SubjectErasure{
			Tables: []string{links},
			Addresses: func(ctx context.Context, host core.Host, accountID string) ([]string, error) {
				held, err := scan(host.Querier(ctx).Query(ctx, `SELECT user_id, tenant_id, address FROM `+links+` WHERE user_id = $1`, accountID))
				var out []string
				for _, h := range held {
					out = append(out, h.Addresses...)
				}
				return out, err
			},
			Holders: func(ctx context.Context, host core.Host, tenant, address string) ([]compliance.SubjectAccount, error) {
				return scan(host.Querier(ctx).Query(ctx, `SELECT user_id, tenant_id, address FROM `+links+` WHERE `+
					compliance.AddressCandidates("address", 1)+` AND tenant_id = $4`, append(compliance.AddressForms(address), tenant)...))
			},
			Erase: func(context.Context, core.Host, string) (int64, error) { return 0, nil },
		})}
		resolve := func(t *testing.T, identifier string) []compliance.SubjectAddress {
			t.Helper()
			ctx, err := compliance.ResolveSubjectAddresses(context.Background(), identifier, erasers)
			require.NoError(t, err)
			return compliance.SubjectAddresses(ctx, identifier)
		}

		t.Run("composed and decomposed", func(t *testing.T) {
			var buf bytes.Buffer
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			assert.ElementsMatch(t, inAcme(at("a-idp")), resolve(t, a.String()), "the address b holds decomposed is not a's to reach")
			out := buf.String()
			assert.Equal(t, 1, strings.Count(out, "held by another account"), "one line per request")
			assert.Contains(t, out, `"count":1`)
			assert.NotContains(t, out, tag+"@", "the log names no address")

			assert.ElementsMatch(t, inAcme(bOwn), resolve(t, b.String()), "the address a holds composed is not b's to reach")
		})

		t.Run("linked identity asserting another account's email", func(t *testing.T) {
			assert.ElementsMatch(t, inAcme(at("c"), at("c-idp")), resolve(t, c.String()), "c's link names d's address")
			assert.ElementsMatch(t, inAcme(at("d-idp")), resolve(t, d.String()), "c's link holds d's own address")
		})

		t.Run("typed address reaches both", func(t *testing.T) {
			assert.ElementsMatch(t, append([]compliance.SubjectAddress{{Address: decomposed}}, inAcme(at("a-idp"), bOwn)...),
				resolve(t, decomposed), "the caller named the address, and both accounts hold it")
			assert.ElementsMatch(t, append([]compliance.SubjectAddress{{Address: at("d")}}, inAcme(at("d-idp"))...),
				resolve(t, at("d")))
		})

		t.Run("another tenant's account does not share it", func(t *testing.T) {
			assert.ElementsMatch(t, inAcme(at("e"), at("e-idp")), resolve(t, e.String()))
			assert.ElementsMatch(t, []compliance.SubjectAddress{{Address: at("g"), Tenant: "globex"}, {Address: at("e"), Tenant: "globex"}},
				resolve(t, g.String()))
		})
	})
}
