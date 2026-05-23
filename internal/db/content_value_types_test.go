//go:build !mutest

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// valueTypesTable is the table the schema engine generates for the schema
// below, with the column types it ships per dialect. The test registry's own
// fixture types are looser, and what matters is what the real types read back
// as.
var valueTypesTable = map[string]string{
	"postgres": `CREATE TABLE "_value_types" (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		tenant_id VARCHAR(64) NOT NULL DEFAULT '',
		title TEXT, price NUMERIC, active BOOLEAN, archived BOOLEAN, missing BOOLEAN,
		born DATE, seen_at TIMESTAMPTZ, naive_at TIMESTAMPTZ, never_at TIMESTAMPTZ, meta JSONB, ref UUID)`,
	"mysql": "CREATE TABLE `_value_types` (" +
		"id CHAR(36) PRIMARY KEY DEFAULT (UUID()), " +
		"created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), " +
		"updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), " +
		"tenant_id VARCHAR(64) NOT NULL DEFAULT '', " +
		"title LONGTEXT, price DECIMAL(38,10), active TINYINT(1), archived TINYINT(1), missing TINYINT(1), " +
		"born DATE, seen_at DATETIME(6), naive_at DATETIME(6), never_at DATETIME(6), meta JSON, ref CHAR(36))",
	"mssql": `CREATE TABLE [_value_types] (
		id UNIQUEIDENTIFIER PRIMARY KEY DEFAULT NEWID(),
		created_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME(),
		updated_at DATETIME2(7) NOT NULL DEFAULT SYSUTCDATETIME(),
		tenant_id NVARCHAR(64) NOT NULL DEFAULT '',
		title NVARCHAR(MAX), price NUMERIC(38,10), active BIT, archived BIT, missing BIT,
		born DATE, seen_at DATETIME2(7), naive_at DATETIME2(7), never_at DATETIME2(7), meta NVARCHAR(MAX), ref UNIQUEIDENTIFIER)`,
}

// The content API answers with the same JSON whichever database holds the
// entry. MySQL's JSON_OBJECT returns a boolean as 1 and a datetime with no
// offset, and SQL Server's FOR JSON returns a datetime with no offset and a
// json field as an escaped string, so the store normalizes what each returns.
func TestContentStore_ReadsTheSameJSONOnEveryDialect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	const want = `{"active":true,"archived":false,"born":"2026-10-01","meta":{"n":1,"tags":["a"]},` +
		`"missing":null,"naive_at":"2026-10-01T09:30:00+00:00","never_at":null,"price":19.9,"ref":"6f1c2b9e-4d3a-4c55-9e1f-2a7b8c9d0e1f",` +
		`"seen_at":"2026-10-01T09:30:00.25+00:00","title":"Widget"}`

	dialects := []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.pool(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, valueTypesTable[d.name]); err != nil {
				t.Fatalf("create table: %v", err)
			}

			reg := newTestRegistry(t, pool)
			sc := &domain.Schema{
				Name: "value_types",
				Fields: []domain.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "price", FieldType: "number"},
					{Name: "active", FieldType: "boolean"},
					{Name: "archived", FieldType: "boolean"},
					{Name: "missing", FieldType: "boolean"},
					{Name: "born", FieldType: "date"},
					{Name: "seen_at", FieldType: "datetime"},
					{Name: "naive_at", FieldType: "datetime"},
					{Name: "never_at", FieldType: "datetime"},
					{Name: "meta", FieldType: "json"},
					{Name: "ref", FieldType: "uid"},
				},
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("register schema: %v", err)
			}
			store := db.NewContentStore(pool, reg.Source())

			created, err := store.Insert(ctx, sc.Name, map[string]any{
				"title":    "Widget",
				"price":    19.9,
				"active":   true,
				"archived": false,
				"born":     "2026-10-01",
				"seen_at":  "2026-10-01T09:30:00.25Z",
				"naive_at": "2026-10-01 09:30:00",
				"meta":     map[string]any{"n": 1, "tags": []any{"a"}},
				"ref":      "6f1c2b9e-4d3a-4c55-9e1f-2a7b8c9d0e1f",
			})
			if err != nil {
				t.Fatalf("insert: %v", err)
			}

			got, err := store.GetByID(ctx, sc.Name, created.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			listed, err := store.List(ctx, sc.Name, 10, 0, nil)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(listed) != 1 {
				t.Fatalf("list returned %d entries, want 1", len(listed))
			}

			for name, data := range map[string]map[string]any{
				"insert": created.Data,
				"get":    got.Data,
				"list":   listed[0].Data,
			} {
				delete(data, "id")
				b, err := json.Marshal(data)
				if err != nil {
					t.Fatalf("%s: marshal: %v", name, err)
				}
				if string(b) != want {
					t.Errorf("%s on %s:\n got %s\nwant %s", name, d.name, b, want)
				}
			}
		})
	}
}
