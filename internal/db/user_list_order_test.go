//go:build !mutest

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// Postgres stamps every row of one transaction with the same created_at, so
// accounts an import creates together leave the list nothing but the tie
// breaker to order by. Without a unique one, offset pages can repeat some
// accounts and skip others. Postgres keeps the scan order for ties until a
// page's offset plus limit is under half the rows, where it switches to a
// bounded heap sort, so the table holds five pages rather than two and a bit.
func TestUserStore_ListPagesEachAccountOnce(t *testing.T) {
	const total, page = 1000, 200
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)

		created := make([]uuid.UUID, 0, total)
		for i := range total {
			u, err := store.Create(ctx, fmt.Sprintf("tied%d@order.test", i), "x", []string{"editor"}, "acme")
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			created = append(created, u.ID)
		}
		if _, err := pool.Exec(ctx, `UPDATE sys_users SET created_at = $1`, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
			t.Fatalf("share created_at: %v", err)
		}

		seen := make(map[uuid.UUID]int, total)
		for offset := 0; ; offset += page {
			got, err := store.ListMatching(ctx, "", "", page, offset)
			if err != nil {
				t.Fatalf("List offset %d: %v", offset, err)
			}
			for _, u := range got {
				seen[u.ID]++
			}
			if len(got) < page {
				break
			}
		}

		var repeated, missing int
		for _, id := range created {
			switch n := seen[id]; {
			case n == 0:
				missing++
			case n > 1:
				repeated++
			}
		}
		if repeated > 0 || missing > 0 {
			t.Fatalf("paging %d accounts by %d: %d distinct listed, %d repeated, %d missing",
				total, page, len(seen), repeated, missing)
		}
	})
}
