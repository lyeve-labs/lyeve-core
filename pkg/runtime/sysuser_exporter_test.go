package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
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
	ctx := context.Background()

	id := uuid.New()
	email := "subject@example.com"
	if _, err := host.Querier(ctx).Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, tenant_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		id, email, "hashed", []string{"editor"}, ""); err != nil {
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

	got, err := exp.ExportSubject(context.Background(), "nobody@example.com")
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

		got, err := exporter.ExportSubject(ctx, id.String())
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		if got != nil {
			t.Fatalf("post-erase export = %#v, want nil", got)
		}
	})
}
