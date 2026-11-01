package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

func TestSysUserExporter_BlankIdentifierReturnsNothing(t *testing.T) {
	exp := newSysUserExporter(plugintest.Postgres(t))

	for _, id := range []string{"", "   "} {
		got, err := exp.ExportSubject(context.Background(), id)
		if err != nil {
			t.Fatalf("ExportSubject(%q): %v", id, err)
		}
		if got != nil {
			t.Errorf("ExportSubject(%q) = %v, want nil", id, got)
		}
	}
}

func TestSysUserExporter_MatchesEmailAndID(t *testing.T) {
	host := plugintest.Postgres(t)
	exp := newSysUserExporter(host)
	ctx := core.WithTenantID(context.Background(), "acme")

	id := uuid.New()
	email := "subject@example.com"
	if _, err := host.Querier(ctx).Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, email, "hashed", []string{"editor"}, "acme"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	for _, identifier := range []string{email, id.String()} {
		got, err := exp.ExportSubject(ctx, identifier)
		if err != nil {
			t.Fatalf("ExportSubject(%q): %v", identifier, err)
		}
		rows, ok := got["sys_users"].([]map[string]any)
		if !ok || len(rows) != 1 {
			t.Fatalf("ExportSubject(%q) = %#v, want one sys_users row", identifier, got)
		}
		if rows[0]["email"] != email {
			t.Errorf("email = %v, want %q", rows[0]["email"], email)
		}
		if _, leaked := rows[0]["password_hash"]; leaked {
			t.Error("export carries password_hash")
		}
	}
}

func TestSysUserExporter_UnknownIdentifierReturnsNothing(t *testing.T) {
	exp := newSysUserExporter(plugintest.Postgres(t))

	got, err := exp.ExportSubject(core.WithTenantID(context.Background(), "acme"), "nobody@example.com")
	if err != nil {
		t.Fatalf("ExportSubject: %v", err)
	}
	if got != nil {
		t.Errorf("ExportSubject = %v, want nil", got)
	}
}

// TestSysUserExporter_ExcludesAnonymizedSubject proves that erasing a subject
// and then exporting by their surviving id returns nothing. The id is left in
// place after erasure (only the PII columns are rewritten), so without the
// anonymized filter a post-erase export would echo the anonymized stub back as
// a record and break the "post-erase export must be empty" contract.
func TestSysUserExporter_ExcludesAnonymizedSubject(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		eraser := newSysUserEraser(host)
		exporter := newSysUserExporter(host)
		q := host.Querier(ctx)

		id := uuid.New()
		email := fmt.Sprintf("gone-%s@example.com", uuid.NewString()[:8])
		if _, err := q.Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash) VALUES ($1, $2, $3)`,
			id, email, "hashed"); err != nil {
			t.Fatalf("seed: %v", err)
		}

		n, err := eraser.EraseSubject(ctx, id.String())
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
		if n != 1 {
			t.Fatalf("erase affected %d rows, want 1", n)
		}

		got, err := exporter.ExportSubject(core.WithTenantID(ctx, core.DefaultTenantSlug), id.String())
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if got != nil {
			t.Fatalf("post-erase export = %#v, want nil", got)
		}
	})
}

// seedTenantLocal creates the account a provider sign-up creates for address
// in tenant, which keeps the address in provider_email.
func seedTenantLocal(t *testing.T, host core.Host, tenant, address string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	acct, created, err := host.(core.TenantAccountProvider).TenantAccounts().CreateTenantAccount(ctx, core.NewTenantAccount{
		TenantID: tenant, Address: address, PasswordHash: "hashed", ProviderSeen: true,
	})
	require.NoError(t, err)
	require.True(t, created)
	return acct.ID
}

// exportedRows runs the exporter for tenant and returns its sys_users rows.
func exportedRows(t *testing.T, host core.Host, tenant, identifier string) []map[string]any {
	t.Helper()
	got, err := newSysUserExporter(host).ExportSubject(core.WithTenantID(context.Background(), tenant), identifier)
	require.NoError(t, err)
	if got == nil {
		return nil
	}
	rows, ok := got["sys_users"].([]map[string]any)
	require.True(t, ok, "%#v", got)
	return rows
}

// One address can hold an account in several tenants. A run for one tenant
// exports that tenant's account and nothing from the others.
func TestSysUserExporter_RunExportsOnlyItsTenantsAccounts(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		address := "scoped-" + uuid.NewString()[:8] + "@agency.example"
		ids := map[string]uuid.UUID{
			"acme":  seedTenantLocal(t, host, "acme", address),
			"brand": seedTenantLocal(t, host, "brand", address),
		}

		for tenant, id := range ids {
			rows := exportedRows(t, host, tenant, address)
			require.Len(t, rows, 1, "tenant %s", tenant)
			assert.Equal(t, tenant, rows[0]["tenant_id"])
			assert.Equal(t, strings.ToLower(id.String()), strings.ToLower(rows[0]["id"].(string)))
		}
		assert.Nil(t, exportedRows(t, host, "other", address), "a tenant holding no account exports none")
		assert.Nil(t, exportedRows(t, host, core.DefaultTenantSlug, address))
	})
}

// An account created before tenant-local accounts keeps the address in email.
// It belongs to its own tenant like any other, so a run for another tenant
// that holds the address in provider_email does not export it.
func TestSysUserExporter_LegacyAccountExportsOnlyInItsTenant(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		address := "legacy-" + uuid.NewString()[:8] + "@agency.example"
		legacy := uuid.New()
		_, err := host.Querier(ctx).Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, tenant_id) VALUES ($1, $2, $3, $4)`,
			legacy, address, "hashed", "acme")
		require.NoError(t, err)
		local := seedTenantLocal(t, host, "brand", address)

		rows := exportedRows(t, host, "acme", address)
		require.Len(t, rows, 1)
		assert.Equal(t, strings.ToLower(legacy.String()), strings.ToLower(rows[0]["id"].(string)))
		assert.Equal(t, address, rows[0]["email"])

		rows = exportedRows(t, host, "brand", address)
		require.Len(t, rows, 1)
		assert.Equal(t, strings.ToLower(local.String()), strings.ToLower(rows[0]["id"].(string)))

		assert.Nil(t, exportedRows(t, host, "acme", local.String()), "an id from another tenant matches nothing")
	})
}

// An account stored with no tenant acts in the default tenant, so the default
// tenant's run exports it and no other run does. A run that names no tenant
// at all cannot say whose accounts it wants and refuses.
func TestSysUserExporter_NoTenantAccountExportsWithTheDefaultTenant(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		address := "first-" + uuid.NewString()[:8] + "@agency.example"
		id := uuid.New()
		_, err := host.Querier(ctx).Exec(ctx,
			`INSERT INTO sys_users (id, email, password_hash, tenant_id) VALUES ($1, $2, $3, $4)`,
			id, address, "hashed", "")
		require.NoError(t, err)

		rows := exportedRows(t, host, core.DefaultTenantSlug, address)
		require.Len(t, rows, 1)
		assert.Equal(t, "", rows[0]["tenant_id"])
		assert.Nil(t, exportedRows(t, host, "acme", address))

		got, err := newSysUserExporter(host).ExportSubject(ctx, address)
		assert.ErrorIs(t, err, core.ErrTenantRequired)
		assert.Nil(t, got)
	})
}

// An all-tenant export runs the exporter once per tenant. It names exactly
// the tenants that hold the address and counts each account once.
func TestSysUserExporter_AllTenantExportMatchesEachHolderOnce(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectExporters()
		t.Cleanup(compliance.ResetSubjectExporters)
		registerSysUserExporter(host)

		address := "sweep-" + uuid.NewString()[:8] + "@agency.example"
		seedTenantLocal(t, host, "acme", address)
		seedTenantLocal(t, host, "brand", address)

		result, err := compliance.RunSubjectExportAcrossTenants(context.Background(), address,
			[]string{"acme", "brand", core.DefaultTenantSlug, "other"}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"acme", "brand"}, result.MatchedTenants)
		assert.Equal(t, 2, result.Summary.TotalRecords)
		assert.False(t, result.Incomplete, "%#v", result.Errors)
		for _, tenant := range []string{"acme", "brand"} {
			rows, ok := result.Tenants[tenant].Plugins["sys_users"].([]map[string]any)
			require.True(t, ok, "tenant %s: %#v", tenant, result.Tenants[tenant].Plugins)
			require.Len(t, rows, 1)
			assert.Equal(t, tenant, rows[0]["tenant_id"])
		}
	})
}

// The export names an account by the id every other answer uses, the lower
// case form. SQL Server renders a UNIQUEIDENTIFIER cast to text in upper case.
func TestSysUserExporter_ExportsTheLowerCaseID(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		address := "case-" + uuid.NewString()[:8] + "@agency.example"
		id := seedTenantLocal(t, host, "acme", address)

		for _, identifier := range []string{address, id.String()} {
			rows := exportedRows(t, host, "acme", identifier)
			require.Len(t, rows, 1, "identifier %s", identifier)
			assert.Equal(t, strings.ToLower(id.String()), rows[0]["id"])
		}
	})
}

// The account is the engine's own record, so its exporter answers for the
// engine's section, which a person can choose like any plugin's.
func TestSysUserExporter_ExportsAsTheEngineSection(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		compliance.ResetSubjectExporters()
		t.Cleanup(compliance.ResetSubjectExporters)
		registerSysUserExporter(host)
		assert.Equal(t, []string{compliance.EngineExportSection}, compliance.SubjectExportSections())

		address := "section-" + uuid.NewString()[:8] + "@agency.example"
		seedTenantLocal(t, host, "acme", address)
		res, err := compliance.RunSubjectExportSections(core.WithTenantID(context.Background(), "acme"), address,
			[]string{compliance.EngineExportSection})
		require.NoError(t, err)
		assert.Equal(t, map[string][]string{compliance.EngineExportSection: {"sys_users"}}, res.Sections)
	})
}
