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

// An erasure reaches the rows kept under every address of the subject: the
// account's own, its identity provider's, and each linked identity's. An
// export handed exporters none of them, so an exporter that keeps rows by
// address listed only what the request named, and an export by account id
// listed nothing. A person asking for their data got less than the erasure
// would remove.

// exportRecorder records the addresses each export run hands it, by tenant.
type exportRecorder struct {
	runs map[string][]compliance.SubjectAddress
}

func (r *exportRecorder) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	r.runs[core.TenantIDFromCtx(ctx)] = compliance.SubjectAddresses(ctx, identifier)
	return nil, nil
}

// linkedAddresses declares the addresses kept in table, one row per linked
// identity, and fails while broken is set.
func linkedAddresses(table string, broken *bool) compliance.SubjectErasure {
	return compliance.SubjectErasure{
		Tables: []string{table},
		Final:  true,
		Addresses: func(ctx context.Context, host core.Host, accountID string) ([]string, error) {
			if *broken {
				return nil, errors.New("links store is down")
			}
			rows, err := host.Querier(ctx).Query(ctx, `SELECT address FROM `+table+` WHERE user_id = $1`, accountID)
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
		Erase: func(context.Context, core.Host, string) (int64, error) { return 0, nil },
	}
}

func TestRunSubjectExport_HandsExportersTheAddressesTheErasureReaches(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		compliance.ResetSubjectErasers()
		t.Cleanup(compliance.ResetSubjectErasers)
		compliance.ResetSubjectExporters()
		t.Cleanup(compliance.ResetSubjectExporters)

		links := "links_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
		_, err := q.Exec(ctx, `CREATE TABLE `+links+` (user_id VARCHAR(36) NOT NULL, address VARCHAR(255) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+links) })

		tag := uuid.NewString()[:8]
		id := uuid.New()
		email, provider, linked := "own-"+tag+"@example.com", "idp-"+tag+"@example.com", "gh-"+tag+"@example.com"
		_, err = q.Exec(ctx, `INSERT INTO sys_users (id, email, password_hash, provider_email) VALUES ($1, $2, '', $3)`, id, email, provider)
		require.NoError(t, err)
		_, err = q.Exec(ctx, `INSERT INTO `+links+` (user_id, address) VALUES ($1, $2)`, id.String(), linked)
		require.NoError(t, err)

		broken := false
		compliance.RegisterSubjectEraser(plugintest.SubjectAccounts(host))
		compliance.RegisterSubjectEraser(compliance.NewDeclaredEraser("links", host, linkedAddresses(links, &broken)))
		rec := &exportRecorder{runs: map[string][]compliance.SubjectAddress{}}
		compliance.RegisterSubjectExporter(rec)
		// The account belongs to the install, so the addresses it holds are
		// reached in the default tenant only.
		home := core.DefaultTenantSlug
		all := []compliance.SubjectAddress{{Address: email, Tenant: home}, {Address: provider, Tenant: home}, {Address: linked, Tenant: home}}

		// Any spelling of the id names the account, as it does for the erasure.
		res, err := compliance.RunSubjectExport(core.WithTenantID(ctx, "acme"), "{"+strings.ToUpper(id.String())+"}")
		require.NoError(t, err)
		assert.False(t, res.Incomplete)
		assert.Equal(t, id.String(), res.Identifier)
		assert.ElementsMatch(t, all, rec.runs["acme"], "a single-tenant export by account id on %s", host.Dialect())

		_, err = compliance.RunSubjectExportAcrossTenants(ctx, strings.ToUpper(email), []string{"acme", "globex"}, nil)
		require.NoError(t, err)
		for _, tenant := range []string{"acme", "globex"} {
			assert.ElementsMatch(t, []compliance.SubjectAddress{{Address: strings.ToUpper(email)}, all[1], all[2]}, rec.runs[tenant],
				"the %s run of a sweep by address on %s", tenant, host.Dialect())
		}

		// A set that cannot be read runs no exporter and returns no bundle,
		// as it erases nothing.
		broken = true
		rec.runs = map[string][]compliance.SubjectAddress{}
		res, err = compliance.RunSubjectExport(core.WithTenantID(ctx, "acme"), id.String())
		require.ErrorIs(t, err, compliance.ErrSubjectAddressesUnresolved)
		assert.Nil(t, res)
		res, err = compliance.RunSubjectExportAcrossTenants(ctx, id.String(), []string{"acme", "globex"}, nil)
		require.ErrorIs(t, err, compliance.ErrSubjectAddressesUnresolved)
		assert.Nil(t, res)
		assert.Empty(t, rec.runs, "an exporter ran without the subject's addresses")
	})
}

// requestTenantRecorder records the request tenant each export run sees, by
// the tenant the run is bound to.
type requestTenantRecorder map[string]string

func (r requestTenantRecorder) ExportSubject(ctx context.Context, _ string) (map[string]any, error) {
	r[core.TenantIDFromCtx(ctx)] = compliance.RequestTenant(ctx)
	return nil, nil
}

// An address no account holds reaches the tenant the request runs in, as
// its erasure does. A sweep binds each run to its own tenant, and nothing
// else told an exporter which tenant the request named, so such an address
// reached every tenant the sweep covered.
func TestRunSubjectExportAcrossTenants_EveryRunSeesTheRequestTenant(t *testing.T) {
	compliance.ResetSubjectErasers()
	t.Cleanup(compliance.ResetSubjectErasers)
	compliance.ResetSubjectExporters()
	t.Cleanup(compliance.ResetSubjectExporters)
	rec := requestTenantRecorder{}
	compliance.RegisterSubjectExporter(rec)

	_, err := compliance.RunSubjectExportAcrossTenants(core.WithTenantID(context.Background(), "acme"),
		"nobody@example.com", []string{"acme", "globex"}, nil)
	require.NoError(t, err)
	assert.Equal(t, requestTenantRecorder{"acme": "acme", "globex": "acme"}, rec)
}
