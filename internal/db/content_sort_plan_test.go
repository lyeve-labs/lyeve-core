//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// lastQuery keeps the text and arguments of the last query it passes on.
type lastQuery struct {
	db.DB
	mu   sync.Mutex
	text string
	args []any
}

func (q *lastQuery) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.mu.Lock()
	q.text, q.args = query, args
	q.mu.Unlock()
	return q.DB.Query(ctx, query, args...)
}

func (q *lastQuery) DatabaseName() string { return db.DatabaseNameOf(q.DB) }

// A sort on created_at reads the tenant index the schema engine gives every
// generated table, in either direction, so a page costs the rows it serves
// rather than a sort of the tenant's whole table. The public delivery list
// sorts on nothing else for that reason.
func TestContentStore_SortOnCreatedAtReadsTheTenantIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	pool := testdb.Postgres(t)
	ctx := tenant.WithID(context.Background(), "t1")
	reg := newTestRegistry(t, pool)
	sc := &domain.Schema{Name: "sort_plan_posts", WithCreatedAt: true, WithUpdatedAt: true,
		Fields: []domain.SchemaField{{Name: "day", FieldType: "date"}}}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("upsert schema: %v", err)
	}
	t.Cleanup(func() { _ = reg.Delete(context.Background(), sc.Name) })
	table := domain.TableName(sc.Name)
	for _, stmt := range []string{
		// The index the schema engine creates on every generated table.
		`CREATE INDEX sort_plan_posts_tenant ON ` + table + ` (tenant_id, created_at)`,
		`INSERT INTO ` + table + ` (tenant_id, created_at, day)
		 SELECT CASE WHEN g % 10 = 0 THEN 't2' ELSE 't1' END, now() - g * interval '1 second', DATE '2026-01-01' + (g % 300)
		 FROM generate_series(1, 20000) g`,
		`ANALYZE ` + table,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	rec := &lastQuery{DB: pool}
	store := db.NewContentStore(rec, reg.Source())
	for _, desc := range []bool{false, true} {
		items, err := store.ListPage(ctx, sc.Name, db.ListQuery{Limit: 20, Offset: 40, Sort: db.ListSort{Column: "created_at", Desc: desc}})
		if err != nil {
			t.Fatalf("ListPage: %v", err)
		}
		if len(items) != 20 {
			t.Fatalf("ListPage served %d items, want 20", len(items))
		}
		rows, err := pool.Query(ctx, "EXPLAIN "+rec.text, rec.args...)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan = append(plan, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("plan rows: %v", err)
		}
		rows.Close()
		text := strings.Join(plan, "\n")
		if !strings.Contains(text, "sort_plan_posts_tenant") || strings.Contains(text, "Seq Scan") {
			t.Errorf("desc=%v: the sort does not read the tenant index:\n%s", desc, text)
		}
	}
}
