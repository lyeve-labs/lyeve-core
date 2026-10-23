package compliance_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// An address can pass from one person to the next, and a person exporting
// their own data saw every row kept under it, the last holder's included.
// Nothing told an exporter that the run was the person's own, or when their
// account began.

// sinceRecorder records what SubjectSince answers in each run.
type sinceRecorder struct {
	since time.Time
	ok    bool
}

func (r *sinceRecorder) ExportSubject(ctx context.Context, _ string) (map[string]any, error) {
	r.since, r.ok = compliance.SubjectSince(ctx)
	return nil, nil
}

func TestSubjectSince_OnlyTheOwnExportCarriesTheAccountCreation(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		compliance.ResetSubjectErasers()
		t.Cleanup(compliance.ResetSubjectErasers)
		compliance.ResetSubjectExporters()
		t.Cleanup(compliance.ResetSubjectExporters)

		// Whole seconds, because MySQL keeps sys_users.created_at to the second.
		created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
		id, elsewhere := uuid.New(), uuid.New()
		for _, a := range []struct {
			id     uuid.UUID
			tenant string
		}{{id, "acme"}, {elsewhere, "globex"}} {
			_, err := host.Querier(ctx).Exec(ctx,
				`INSERT INTO sys_users (id, email, password_hash, tenant_id, created_at) VALUES ($1, $2, '', $3, $4)`,
				a.id, "since-"+a.id.String()[:8]+"@example.com", a.tenant, created)
			require.NoError(t, err)
		}
		compliance.RegisterSubjectEraser(plugintest.SubjectAccounts(host))
		rec := &sinceRecorder{}
		compliance.RegisterSubjectExporterAs("notes", rec)
		acme := core.WithTenantID(ctx, "acme")

		_, err := compliance.RunSubjectExportSections(acme, id.String(), []string{"notes"})
		require.NoError(t, err)
		require.True(t, rec.ok, "the person's own export on %s", host.Dialect())
		assert.True(t, created.Equal(rec.since), "the account's creation, got %s want %s", rec.since, created)

		_, err = compliance.RunSubjectExport(acme, id.String())
		require.NoError(t, err)
		assert.False(t, rec.ok, "the super admin's export keeps every row")

		// An account of another tenant has no creation in this one, so the
		// run holds nothing found through an address.
		before := time.Now()
		_, err = compliance.RunSubjectExportSections(acme, elsewhere.String(), []string{"notes"})
		require.NoError(t, err)
		require.True(t, rec.ok)
		assert.False(t, rec.since.Before(before), "an account of another tenant bounds the run at now")
	})
}
