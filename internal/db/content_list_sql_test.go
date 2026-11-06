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
)

// listRecorder keeps the text of every list query it passes on.
type listRecorder struct {
	db.DB
	mu    sync.Mutex
	texts map[string]int
}

func (r *listRecorder) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "ORDER BY created_at DESC, id DESC") {
		r.mu.Lock()
		r.texts[query]++
		r.mu.Unlock()
	}
	return r.DB.Query(ctx, query, args...)
}

// DatabaseName forwards the pool's own database name, which MySQL and SQL
// Server need to qualify a generated table.
func (r *listRecorder) DatabaseName() string { return db.DatabaseNameOf(r.DB) }

// One set of filters is one statement text. The connection's statement cache
// and the server's plan cache match by that text, so a text that follows the
// map's walk order prepares and plans the same query once per order.
func TestContentStore_ListBuildsOneStatementPerFilterSet(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg := newTestRegistry(t, pool)
		sc := &domain.Schema{Name: "stable_list_sql", Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "slug", FieldType: "text"},
			{Name: "kind", FieldType: "text"},
		}}
		if err := reg.Upsert(ctx, sc); err != nil {
			t.Fatalf("upsert schema: %v", err)
		}
		t.Cleanup(func() { _ = reg.Delete(ctx, sc.Name) })

		rec := &listRecorder{DB: pool, texts: map[string]int{}}
		store := db.NewContentStore(rec, reg.Source())
		for range 100 {
			filters := map[string]any{"title": "a", "slug": "b", "kind": "c"}
			if _, err := store.List(ctx, sc.Name, 10, 0, filters); err != nil {
				t.Fatalf("List: %v", err)
			}
		}
		if len(rec.texts) != 1 {
			t.Errorf("one filter set built %d statement texts:", len(rec.texts))
			for text, n := range rec.texts {
				t.Logf("%d times: %s", n, text)
			}
		}
	})
}

// A list plan follows the definition it was built from. A field added after
// a list is one a filter may name and a row carries, so a plan that outlived
// the change would refuse the filter as an unknown key.
func TestContentStore_ListFiltersOnAFieldAddedAfterAList(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg := newTestRegistry(t, pool)
		sc := &domain.Schema{Name: "list_plan_field_added", Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		}}
		if err := reg.Upsert(ctx, sc); err != nil {
			t.Fatalf("upsert schema: %v", err)
		}
		t.Cleanup(func() { _ = reg.Delete(ctx, sc.Name) })

		store := db.NewContentStore(pool, reg.Source())
		if _, err := store.List(ctx, sc.Name, 10, 0, map[string]any{"title": "a"}); err != nil {
			t.Fatalf("List before the change: %v", err)
		}

		grown := &domain.Schema{Name: sc.Name, Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "kind", FieldType: "text"},
		}}
		if err := reg.Upsert(ctx, grown); err != nil {
			t.Fatalf("add a field: %v", err)
		}
		// The test engine only creates tables, so the column the schema
		// engine would add is added here.
		if _, err := pool.Exec(ctx, "ALTER TABLE "+domain.TableName(sc.Name)+" ADD kind VARCHAR(255)"); err != nil {
			t.Fatalf("add the column: %v", err)
		}
		if _, err := store.Insert(ctx, sc.Name, map[string]any{"title": "a", "kind": "b"}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		items, err := store.List(ctx, sc.Name, 10, 0, map[string]any{"kind": "b"})
		if err != nil {
			t.Fatalf("List on the added field: %v", err)
		}
		if len(items) != 1 || items[0].Data["kind"] != "b" {
			t.Fatalf("List on the added field answered %d items: %+v", len(items), items)
		}
	})
}
