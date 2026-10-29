//go:build !mutest

package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Postgres stamps every row of one transaction with the same created_at, so a
// bulk create larger than a page leaves the list nothing but the tie breaker
// to order by. Without a unique one, offset pages can repeat some rows and
// skip others. Postgres keeps the scan order for ties until a page's offset
// plus limit is under half the rows, where it switches to a bounded heap sort,
// so the table holds five pages rather than two and a bit.
func TestContentStore_ListPagesEachEntryOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	const total, page = 1000, 200
	for _, name := range []string{"postgres", "mysql", "mssql"} {
		t.Run(name, func(t *testing.T) {
			if !testdb.ShouldTest(name) {
				t.Skipf("CI_DIALECT != %s", name)
			}
			store, sc := statusFixture(t, name, false)
			ctx := context.Background()

			items := make([]map[string]any, total)
			for i := range items {
				items[i] = map[string]any{"title": "t"}
			}
			created, err := store.BulkInsert(ctx, sc.Name, items)
			if err != nil {
				t.Fatalf("BulkInsert: %v", err)
			}

			seen := make(map[uuid.UUID]int, total)
			for offset := 0; offset < total; offset += page {
				got, err := store.List(ctx, sc.Name, page, offset, nil)
				if err != nil {
					t.Fatalf("List offset %d: %v", offset, err)
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
			if repeated > 0 || missing > 0 || len(seen) != total {
				t.Fatalf("paging %d entries by %d: %d distinct listed, %d repeated, %d missing",
					total, page, len(seen), repeated, missing)
			}
		})
	}
}
