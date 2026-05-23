//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
)

// Query-counting spy

// countingDB wraps a db.DB to count Query, QueryRow, and Exec calls.
type countingDB struct {
	db.DB
	mu            sync.Mutex
	queryCalls    int
	queryRowCalls int
	execCalls     int
}

func (c *countingDB) Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	c.mu.Lock()
	c.queryCalls++
	c.mu.Unlock()
	return c.DB.Query(ctx, sql, args...)
}

func (c *countingDB) QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error) {
	c.mu.Lock()
	c.queryRowCalls++
	c.mu.Unlock()
	return c.DB.QueryRow(ctx, sql, args...)
}

func (c *countingDB) Exec(ctx context.Context, sql string, args ...any) (sql.Result, error) {
	c.mu.Lock()
	c.execCalls++
	c.mu.Unlock()
	return c.DB.Exec(ctx, sql, args...)
}

func (c *countingDB) reset() {
	c.mu.Lock()
	c.queryCalls = 0
	c.queryRowCalls = 0
	c.execCalls = 0
	c.mu.Unlock()
}

// Test: query count is independent of item count

func TestPopulateQueryCount(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	counter := &countingDB{DB: pool}
	ctx := context.Background()

	// The tables and the registry come from a supplied schema engine, because
	// this module has neither. It is built over the plain pool rather than the
	// counter on purpose: creating a table is not a populate query, and
	// counting it would hide what this test measures.
	engine := schemaengine.New(testhost.New(pool))

	// Create schemas

	authorSchema := &domain.Schema{
		Name:             "popq_author",
		Fields:           []domain.SchemaField{{Name: "name", FieldType: "text"}},
		WithDraftPublish: false,
	}
	mustApply(t, engine, authorSchema)

	categorySchema := &domain.Schema{
		Name:             "popq_category",
		Fields:           []domain.SchemaField{{Name: "label", FieldType: "text"}},
		WithDraftPublish: false,
	}
	mustApply(t, engine, categorySchema)

	postSchema := &domain.Schema{
		Name: "popq_post",
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{
				Name:         "author",
				FieldType:    "relation",
				RelationTo:   "popq_author",
				RelationType: domain.RelBelongsTo,
			},
			{
				Name:         "category",
				FieldType:    "relation",
				RelationTo:   "popq_category",
				RelationType: domain.RelBelongsTo,
			},
		},
		WithDraftPublish: false,
	}
	mustApply(t, engine, postSchema)

	store := db.NewContentStore(counter, engine.SchemaSource())

	// Seed data

	// 5 authors, 3 categories: small pool that items cycle through.
	var authorIDs []string
	for i := range 5 {
		c, err := store.Insert(ctx, "popq_author", map[string]any{"name": fmt.Sprintf("A%d", i)})
		if err != nil {
			t.Fatalf("Insert author %d: %v", i, err)
		}
		authorIDs = append(authorIDs, c.ID.String())
	}

	var catIDs []string
	for i := range 3 {
		c, err := store.Insert(ctx, "popq_category", map[string]any{"label": fmt.Sprintf("C%d", i)})
		if err != nil {
			t.Fatalf("Insert category %d: %v", i, err)
		}
		catIDs = append(catIDs, c.ID.String())
	}

	const numPosts = 20
	for i := range numPosts {
		_, err := store.Insert(ctx, "popq_post", map[string]any{
			"title":    fmt.Sprintf("Post %d", i),
			"author":   authorIDs[i%len(authorIDs)],
			"category": catIDs[i%len(catIDs)],
		})
		if err != nil {
			t.Fatalf("Insert post %d: %v", i, err)
		}
	}

	// Fetch all posts (also warms the schema cache via List -> s.table -> GetByName).
	allPosts, err := store.List(ctx, "popq_post", numPosts+10, 0, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(allPosts) != numPosts {
		t.Fatalf("List returned %d, want %d", len(allPosts), numPosts)
	}

	cfg := domain.PopulateConfig{Paths: []string{"author", "category"}}

	// Measure with 5 items
	five := allPosts[:5]
	counter.reset()
	if err := store.PopulateWithConfigBatch(ctx, five, cfg); err != nil {
		t.Fatalf("PopulateWithConfigBatch (5 items): %v", err)
	}
	q5 := counter.queryCalls
	qr5 := counter.queryRowCalls
	e5 := counter.execCalls

	// Measure with 20 items
	counter.reset()
	if err := store.PopulateWithConfigBatch(ctx, allPosts, cfg); err != nil {
		t.Fatalf("PopulateWithConfigBatch (20 items): %v", err)
	}
	q20 := counter.queryCalls
	qr20 := counter.queryRowCalls
	e20 := counter.execCalls

	// Verify results
	for _, p := range allPosts {
		author, ok := p.Data["author"].(map[string]any)
		if !ok || author == nil {
			t.Errorf("post %s: author not populated", p.ID)
		}
		category, ok := p.Data["category"].(map[string]any)
		if !ok || category == nil {
			t.Errorf("post %s: category not populated", p.ID)
		}
	}

	t.Logf("5 items:  query=%d queryRow=%d exec=%d total=%d", q5, qr5, e5, q5+qr5+e5)
	t.Logf("20 items: query=%d queryRow=%d exec=%d total=%d", q20, qr20, e20, q20+qr20+e20)

	// Assertions
	//
	// With batch populate + schema cache:
	//   Per recursion level with R relation fields:
	//     - 1 GetByName (served from cache after warmup -> 0 DB calls)
	//     - R data queries (GetByIDs with IN (...) -> 1 Query each)
	//   Total: exactly R Query calls, 0 QueryRow, 0 Exec.
	//
	// For 2 belongs_to relations: exactly 2 Query calls.
	// This count MUST be identical for 5 items and 20 items.

	const expectedDataQueries = 2 // one per relation field

	if q5 != expectedDataQueries {
		t.Errorf("5 items: Query calls = %d, want %d", q5, expectedDataQueries)
	}
	if q20 != expectedDataQueries {
		t.Errorf("20 items: Query calls = %d, want %d", q20, expectedDataQueries)
	}

	// No schema lookups: GetByName is served from cache.
	if qr5 != 0 {
		t.Errorf("5 items: QueryRow calls = %d, want 0 "+
			"(GetByName should hit cache; non-zero means N+1 per-item schema lookups)", qr5)
	}
	if qr20 != 0 {
		t.Errorf("20 items: QueryRow calls = %d, want 0 "+
			"(GetByName should hit cache; non-zero means N+1 per-item schema lookups)", qr20)
	}

	// Populate is read-only.
	if e5 != 0 || e20 != 0 {
		t.Errorf("Exec calls: 5=%d, 20=%d, want 0 (populate is read-only)", e5, e20)
	}

	// Query count is independent of item count: the core assertion.
	if q5 != q20 {
		t.Errorf("Query calls differ: 5 items=%d, 20 items=%d - "+
			"batch populate should issue the same number of queries regardless of item count", q5, q20)
	}
	if qr5 != qr20 {
		t.Errorf("QueryRow calls differ: 5 items=%d, 20 items=%d", qr5, qr20)
	}
}

// helpers

func mustApply(t *testing.T, engine *schemaengine.Engine, sc *domain.Schema) {
	t.Helper()
	def, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal %s: %v", sc.Name, err)
	}
	if err := engine.Apply(context.Background(), sc.Name, def); err != nil {
		t.Fatalf("engine.Apply %s: %v", sc.Name, err)
	}
}
