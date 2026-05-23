//go:build !mutest

package db_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// A json field holds whatever the caller unmarshaled, which for a nested
// object is a map[string]any. pgx encodes one into jsonb by itself. The MySQL
// and MSSQL drivers bind only scalars and reject a map, so the value is
// re-encoded before binding. Every dialect has to take the same value.
func TestContentStore_JSONFieldAcceptsNestedValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	dialects := []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}

	cases := []struct {
		name  string
		value any
	}{
		{"object", map[string]any{"author": "ada", "tags": []any{"x", "y"}}},
		{"array", []any{1.0, 2.0, 3.0}},
		{"string", `{"already":"encoded"}`},
		{"raw", json.RawMessage(`{"pre":"encoded"}`)},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.pool(t)
			ctx := context.Background()

			reg := newTestRegistry(t, pool)
			sc := &domain.Schema{
				Name: "json_field_entries",
				Fields: []domain.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "meta", FieldType: "json"},
					// The admin mirror flattens an entry body into top-level
					// keys, so a nested object can land on a field the schema
					// never declared as json. No driver can bind a map to any
					// column type, so that has to work too.
					{Name: "extra", FieldType: "json"},
				},
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("engine.Apply: %v", err)
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("upsert schema: %v", err)
			}
			store := db.NewContentStore(pool, reg.Source())

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					row := map[string]any{"title": tc.name, "meta": tc.value, "extra": tc.value}
					if _, err := store.Insert(ctx, sc.Name, row); err != nil {
						t.Fatalf("insert %s: %v", tc.name, err)
					}

					// The mirror path the admin write uses: upsert by a known
					// id, which updates when the row is already there.
					id := uuid.New()
					if err := store.UpsertContent(ctx, sc.Name, id, row); err != nil {
						t.Fatalf("upsert insert %s: %v", tc.name, err)
					}
					if err := store.UpsertContent(ctx, sc.Name, id, row); err != nil {
						t.Fatalf("upsert update %s: %v", tc.name, err)
					}
				})
			}
		})
	}
}
