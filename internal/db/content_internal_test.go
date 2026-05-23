//go:build !mutest

package db

import (
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

func TestDataColumnExpr_Postgres(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "order", FieldType: "number"},
			{Name: "group", FieldType: "text"},
		},
	}
	expr := dataColumnExpr("postgres", sc, "r")

	// PG uses to_jsonb: no JSON_OBJECT, no dialect quoting needed.
	if !strings.Contains(expr, "to_jsonb(r)") {
		t.Errorf("expected to_jsonb(r) in expression, got: %s", expr)
	}
	if strings.Contains(expr, "JSON_OBJECT") {
		t.Errorf("PG should not use JSON_OBJECT, got: %s", expr)
	}
}

func TestDataColumnExpr_MySQL_QuotesReservedWords(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "order", FieldType: "number"},
			{Name: "group", FieldType: "text"},
		},
		WithSoftDelete:   true,
		WithDraftPublish: true,
		WithLocalization: true,
	}
	expr := dataColumnExpr("mysql", sc, "r")

	if !strings.Contains(expr, "JSON_OBJECT(") {
		t.Fatalf("expected JSON_OBJECT wrapper, got: %s", expr)
	}
	// Reserved words must be backtick-quoted.
	if !strings.Contains(expr, "`order`") {
		t.Errorf("'order' not backtick-quoted in: %s", expr)
	}
	if !strings.Contains(expr, "`group`") {
		t.Errorf("'group' not backtick-quoted in: %s", expr)
	}
	// Non-reserved words should also be quoted (consistency).
	if !strings.Contains(expr, "`title`") {
		t.Errorf("'title' not backtick-quoted in: %s", expr)
	}
	// System columns should be quoted too.
	if !strings.Contains(expr, "`deleted_at`") {
		t.Errorf("'deleted_at' not backtick-quoted in: %s", expr)
	}
	if !strings.Contains(expr, "`_status`") {
		t.Errorf("'_status' not backtick-quoted in: %s", expr)
	}
	if !strings.Contains(expr, "`_locale`") {
		t.Errorf("'_locale' not backtick-quoted in: %s", expr)
	}
	// JSON key strings should remain single-quoted, not backtick-quoted.
	if strings.Contains(expr, "`'") || strings.Contains(expr, "'`") {
		t.Errorf("JSON keys should be single-quoted only, got mixed quoting: %s", expr)
	}
}

func TestDataColumnExpr_MSSQL_QuotesReservedWords(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "order", FieldType: "number"},
			{Name: "key", FieldType: "text"},
		},
		WithSoftDelete: true,
	}
	expr := dataColumnExpr("mssql", sc, "r")

	if !strings.Contains(expr, "FOR JSON PATH") {
		t.Fatalf("expected FOR JSON PATH projection, got: %s", expr)
	}
	// Reserved words must be bracket-quoted.
	if !strings.Contains(expr, "[order]") {
		t.Errorf("'order' not bracket-quoted in: %s", expr)
	}
	if !strings.Contains(expr, "[key]") {
		t.Errorf("'key' not bracket-quoted in: %s", expr)
	}
	// Non-reserved words should also be quoted.
	if !strings.Contains(expr, "[title]") {
		t.Errorf("'title' not bracket-quoted in: %s", expr)
	}
	if !strings.Contains(expr, "[deleted_at]") {
		t.Errorf("'deleted_at' not bracket-quoted in: %s", expr)
	}
	// JSON key strings should remain single-quoted.
	if strings.Contains(expr, "['") || strings.Contains(expr, "']") {
		t.Errorf("JSON keys should be single-quoted only, got mixed quoting: %s", expr)
	}
}

func TestDataColumnExpr_SkipsSystemFields(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "id", System: true, FieldType: "uuid"},
			{Name: "created_at", System: true, FieldType: "timestamp"},
		},
	}
	expr := dataColumnExpr("mysql", sc, "r")

	if strings.Contains(expr, "`id`") {
		t.Errorf("system field 'id' should not appear in: %s", expr)
	}
	if strings.Contains(expr, "`created_at`") {
		t.Errorf("system field 'created_at' should not appear in: %s", expr)
	}
	if !strings.Contains(expr, "`title`") {
		t.Errorf("non-system field 'title' should appear in: %s", expr)
	}
}

func TestDataColumnExpr_NoOptionalColumns_EmptySchema(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		},
	}
	expr := dataColumnExpr("mysql", sc, "r")

	if strings.Contains(expr, "deleted_at") {
		t.Errorf("should not contain deleted_at when WithSoftDelete is false: %s", expr)
	}
	// Should contain the field.
	if !strings.Contains(expr, "`title`") {
		t.Errorf("field 'title' should appear in: %s", expr)
	}
}

func TestDataColumnExpr_SQLInjection(t *testing.T) {
	// Even if a malicious field name looks like SQL, QuoteIdentifier
	// neutralizes it by escaping embedded quote characters.
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "a`;DROP TABLE users;--", FieldType: "text"},
		},
	}
	expr := dataColumnExpr("mysql", sc, "r")

	// The backtick in the field name should be doubled by QuoteIdentifier.
	if !strings.Contains(expr, "a``;DROP TABLE users;--") {
		t.Errorf("field name with backtick not properly escaped in: %s", expr)
	}
	// The plain malicious payload should NOT appear.
	if strings.Contains(expr, "DROP TABLE") && !strings.Contains(expr, "``") {
		t.Errorf("potential SQL injection vector in: %s", expr)
	}
}

func TestBuildInsert_QuotesReservedWords(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "order", FieldType: "number"},
			{Name: "group", FieldType: "text"},
			{Name: "title", FieldType: "text"},
		},
	}
	data := map[string]any{"order": 1, "group": "a", "title": "hello"}

	t.Run("mysql", func(t *testing.T) {
		cols, _ := buildInsert(sc, data, dialect.Must("mysql"))
		if got, want := len(cols), 3; got != want {
			t.Fatalf("expected %d cols, got %d", want, got)
		}
		if cols[0] != "`order`" {
			t.Errorf("'order' not backtick-quoted: %q", cols[0])
		}
		if cols[1] != "`group`" {
			t.Errorf("'group' not backtick-quoted: %q", cols[1])
		}
		if cols[2] != "`title`" {
			t.Errorf("'title' not backtick-quoted: %q", cols[2])
		}
	})

	t.Run("mssql", func(t *testing.T) {
		cols, _ := buildInsert(sc, data, dialect.Must("mssql"))
		if cols[0] != "[order]" {
			t.Errorf("'order' not bracket-quoted: %q", cols[0])
		}
		if cols[1] != "[group]" {
			t.Errorf("'group' not bracket-quoted: %q", cols[1])
		}
	})

	t.Run("postgres", func(t *testing.T) {
		cols, _ := buildInsert(sc, data, dialect.Must("postgres"))
		if cols[0] != `"order"` {
			t.Errorf("'order' not double-quoted: %q", cols[0])
		}
	})
}

func TestBuildUpdate_QuotesReservedWords(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "key", FieldType: "text"},
			{Name: "select", FieldType: "number"},
		},
	}
	data := map[string]any{"key": "val", "select": 42}

	t.Run("mysql", func(t *testing.T) {
		sets, _ := buildUpdate(sc, data, dialect.Must("mysql"))
		if got, want := len(sets), 2; got != want {
			t.Fatalf("expected %d SET clauses, got %d", want, got)
		}
		if !strings.Contains(sets[0], "`key`") {
			t.Errorf("'key' not backtick-quoted: %q", sets[0])
		}
		if !strings.Contains(sets[1], "`select`") {
			t.Errorf("'select' not backtick-quoted: %q", sets[1])
		}
	})

	t.Run("mssql", func(t *testing.T) {
		sets, _ := buildUpdate(sc, data, dialect.Must("mssql"))
		if !strings.Contains(sets[0], "[key]") {
			t.Errorf("'key' not bracket-quoted: %q", sets[0])
		}
		if !strings.Contains(sets[1], "[select]") {
			t.Errorf("'select' not bracket-quoted: %q", sets[1])
		}
	})

	t.Run("postgres", func(t *testing.T) {
		sets, _ := buildUpdate(sc, data, dialect.Must("postgres"))
		if !strings.Contains(sets[0], `"key"`) {
			t.Errorf("'key' not double-quoted: %q", sets[0])
		}
	})
}

func TestBuildInsert_PostgresReservedWordIsOneColumn(t *testing.T) {
	// A reserved-word field still maps to exactly one column on Postgres.
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "order", FieldType: "number"},
		},
	}
	cols, _ := buildInsert(sc, map[string]any{"order": 1}, dialect.Must("postgres"))
	if len(cols) != 1 {
		t.Fatalf("expected 1 col, got %d", len(cols))
	}
}
