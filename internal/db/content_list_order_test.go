//go:build !mutest

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Postgres stamps every row of one transaction with the same created_at, so a
// bulk create larger than a page leaves the list nothing but the tie breaker
// to order by. Without a unique one, offset pages can repeat some rows and
// skip others.
//
// The red case depends on the plan. The test fixture builds no (tenant_id,
// created_at) index, so Postgres reads the table in full and sorts it. That
// sort keeps the scan order for ties until a page's offset plus limit is under
// half the rows, where it switches to a bounded heap sort that does not. The
// table holds five pages rather than two and a bit so that a later page meets
// that condition.
const orderTotal, orderPage = 1000, 200

func TestContentStore_ListPagesEachEntryOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			store, sc := statusFixture(t, name, false)
			ctx := context.Background()

			created, err := store.BulkInsert(ctx, sc.Name, titledRows(orderTotal))
			if err != nil {
				t.Fatalf("BulkInsert: %v", err)
			}
			assertPagedOnce(t, created, func(offset int) ([]*domain.Content, error) {
				return store.List(ctx, sc.Name, orderPage, offset, nil)
			})
		})
	}
}

// A many_to_many relation pages its targets the same way, and targets written
// by one bulk create tie on created_at the same way.
func TestContentStore_ListRelatedPagesEachEntryOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg := newTestRegistry(t, pool)
		rel := domain.SchemaField{Name: "tags", FieldType: "relation", RelationType: domain.RelManyToMany, RelationTo: "order_tags"}
		for _, sc := range []*domain.Schema{
			{Name: "order_tags", Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}}},
			{Name: "order_posts", Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}, rel}},
		} {
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("upsert schema %s: %v", sc.Name, err)
			}
			name := sc.Name
			t.Cleanup(func() { _ = reg.Delete(ctx, name) })
		}
		store := db.NewContentStore(pool, reg.Source())

		created, err := store.BulkInsert(ctx, "order_tags", titledRows(orderTotal))
		if err != nil {
			t.Fatalf("BulkInsert: %v", err)
		}
		post, err := store.Insert(ctx, "order_posts", map[string]any{"title": "t"})
		if err != nil {
			t.Fatalf("Insert post: %v", err)
		}
		ids := make([]uuid.UUID, len(created))
		for i, c := range created {
			ids[i] = c.ID
		}
		if err := store.SetRelations(ctx, "order_posts", post.ID, rel, ids); err != nil {
			t.Fatalf("SetRelations: %v", err)
		}
		assertPagedOnce(t, created, func(offset int) ([]*domain.Content, error) {
			items, _, err := store.ListRelated(ctx, "order_posts", post.ID, rel, orderPage, offset, true)
			return items, err
		})
	})
}

func titledRows(n int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{"title": "t"}
	}
	return rows
}

func assertPagedOnce(t *testing.T, created []*domain.Content, page func(offset int) ([]*domain.Content, error)) {
	t.Helper()
	seen := make(map[uuid.UUID]int, len(created))
	for offset := 0; offset < len(created); offset += orderPage {
		got, err := page(offset)
		if err != nil {
			t.Fatalf("page at offset %d: %v", offset, err)
		}
		for _, c := range got {
			seen[c.ID]++
		}
	}
	var repeated, missing int
	for _, c := range created {
		switch n := seen[c.ID]; {
		case n == 0:
			missing++
		case n > 1:
			repeated++
		}
	}
	if repeated > 0 || missing > 0 || len(seen) != len(created) {
		t.Fatalf("paging %d entries by %d: %d distinct listed, %d repeated, %d missing",
			len(created), orderPage, len(seen), repeated, missing)
	}
}
