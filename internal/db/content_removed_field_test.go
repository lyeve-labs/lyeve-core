//go:build !mutest

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Removing a field queues its column drop, so the column and its values stay
// until an admin applies it. The definition is what a reader is served, so
// the field leaves every read at once, on every dialect. Adding the field
// back serves the kept values again. A field named d shares its name with
// the alias the PostgreSQL projection gives the row, and has to stay a field.
func TestContentStore_RemovedFieldLeavesReads(t *testing.T) {
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

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			pool := d.pool(t)
			ctx := context.Background()
			reg := newTestRegistry(t, pool)

			full := &domain.Schema{
				Name: "removed_field_entries",
				Fields: []domain.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "d", FieldType: "text"},
					{Name: "secret", FieldType: "text"},
				},
			}
			if err := reg.Upsert(ctx, full); err != nil {
				t.Fatalf("apply full schema: %v", err)
			}
			store := db.NewContentStore(pool, reg.Source())
			created, err := store.Insert(ctx, full.Name, map[string]any{"title": "kept", "d": "x", "secret": "hidden"})
			if err != nil {
				t.Fatalf("insert: %v", err)
			}

			// The test engine creates a table only when it is missing, so the
			// secret column stays, as it does while a drop is pending.
			trimmed := &domain.Schema{
				Name:   full.Name,
				Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}, {Name: "d", FieldType: "text"}},
			}
			if err := reg.Upsert(ctx, trimmed); err != nil {
				t.Fatalf("apply trimmed schema: %v", err)
			}

			assertServed := func(t *testing.T, how string, c *domain.Content) {
				t.Helper()
				if c.Data["title"] != "kept" {
					t.Errorf("%s: title = %v, want kept", how, c.Data["title"])
				}
				if c.Data["d"] != "x" {
					t.Errorf("%s: d = %v, want x", how, c.Data["d"])
				}
				if v, ok := c.Data["secret"]; ok {
					t.Errorf("%s: removed field secret served as %v", how, v)
				}
			}

			got, err := store.GetByID(ctx, full.Name, created.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			assertServed(t, "GetByID", got)

			list, err := store.List(ctx, full.Name, 10, 0, nil)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("list returned %d rows, want 1", len(list))
			}
			assertServed(t, "List", list[0])

			page, err := store.ListCursor(ctx, full.Name, "", 10, false)
			if err != nil {
				t.Fatalf("list cursor: %v", err)
			}
			if len(page) != 1 {
				t.Fatalf("cursor page returned %d rows, want 1", len(page))
			}
			assertServed(t, "ListCursor", page[0])

			updated, err := store.Update(ctx, full.Name, created.ID, map[string]any{"title": "kept"}, time.Time{})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			assertServed(t, "Update", updated)

			if err := reg.Upsert(ctx, full); err != nil {
				t.Fatalf("apply full schema again: %v", err)
			}
			back, err := store.GetByID(ctx, full.Name, created.ID)
			if err != nil {
				t.Fatalf("get after re-adding: %v", err)
			}
			if back.Data["secret"] != "hidden" {
				t.Errorf("re-added field secret = %v, want hidden", back.Data["secret"])
			}
		})
	}
}
