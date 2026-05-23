package db_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// setupCachedContentStore returns a store backed by real in-memory caches, with
// rows already seeded, plus the schema name.
func setupCachedContentStore(t *testing.T, schemaName string, rows int) (*db.ContentStore, string) {
	t.Helper()

	pool := testdb.Postgres(t)
	ctx := context.Background()
	reg := newTestRegistry(t, pool)

	sc := &domain.Schema{
		Name: schemaName,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "attributes", FieldType: "json"},
		},
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("reg.Upsert: %v", err)
	}
	t.Cleanup(func() { _ = reg.Delete(ctx, schemaName) })

	seeder := db.NewContentStore(pool, reg.Source())
	for i := 0; i < rows; i++ {
		_, err := seeder.Insert(ctx, schemaName, map[string]any{
			"title":      "row",
			"attributes": map[string]any{"nested": map[string]any{"depth": 2}},
		})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	itemCache := cache.NewMemory[string, *domain.Content](100, 0)
	listCache := cache.NewMemory[string, []*domain.Content](100, 0)
	return db.NewContentStoreWithCache(pool, reg.Source(), itemCache, listCache), schemaName
}

// A cached page must not be the caller's page. Relation population writes the
// resolved record into Data[field], and the API layer decorates Data further, so
// a cache that hands out the object it stored lets one request's decoration
// appear in the next request's response.
func TestContentStore_CachedList_DoesNotShareDataWithCallers(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store, schemaName := setupCachedContentStore(t, "cache_isolation", 3)
	ctx := context.Background()

	// Priming read: a miss, which is also what fills the cache.
	first, err := store.List(ctx, schemaName, 25, 0, nil)
	if err != nil {
		t.Fatalf("List (priming): %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(first))
	}

	// Decorate the page the way population does.
	first[0].Data["author"] = map[string]any{"title": "resolved by the first caller"}
	nested, _ := first[0].Data["attributes"].(map[string]any)
	if nested == nil {
		t.Fatal("attributes did not decode as a map, so the fixture does not exercise nesting")
	}
	nested["injected"] = true

	// Cached read: must be untouched by the decoration above.
	second, err := store.List(ctx, schemaName, 25, 0, nil)
	if err != nil {
		t.Fatalf("List (cached): %v", err)
	}
	if _, leaked := second[0].Data["author"]; leaked {
		t.Error("a relation written by an earlier caller reached a later one through the cache")
	}
	if secondNested, ok := second[0].Data["attributes"].(map[string]any); ok {
		if _, leaked := secondNested["injected"]; leaked {
			t.Error("a nested value written by an earlier caller reached a later one through the cache")
		}
	}

	// Two cached reads must not share with each other either.
	a, err := store.List(ctx, schemaName, 25, 0, nil)
	if err != nil {
		t.Fatalf("List (a): %v", err)
	}
	b, err := store.List(ctx, schemaName, 25, 0, nil)
	if err != nil {
		t.Fatalf("List (b): %v", err)
	}
	a[0].Data["only_in_a"] = true
	if _, leaked := b[0].Data["only_in_a"]; leaked {
		t.Error("two callers of the same cached page share one Data map")
	}
}

func TestContentStore_CachedGetByID_DoesNotShareDataWithCallers(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store, schemaName := setupCachedContentStore(t, "cache_isolation_item", 1)
	ctx := context.Background()

	page, err := store.List(ctx, schemaName, 25, 0, nil)
	if err != nil || len(page) != 1 {
		t.Fatalf("seed list: %v", err)
	}
	id := page[0].ID

	first, err := store.GetByID(ctx, schemaName, id)
	if err != nil {
		t.Fatalf("GetByID (priming): %v", err)
	}
	first.Data["author"] = map[string]any{"title": "resolved by the first caller"}

	second, err := store.GetByID(ctx, schemaName, id)
	if err != nil {
		t.Fatalf("GetByID (cached): %v", err)
	}
	if _, leaked := second.Data["author"]; leaked {
		t.Error("a relation written by an earlier caller reached a later one through the item cache")
	}
}

// Concurrent readers of one cached page, some decorating Data the way
// population does and some marshaling it the way ETag generation does. A
// shared object would abort the process with a fatal error no recover can
// catch. Run under -race.
func TestContentStore_CachedList_ConcurrentDecorateAndMarshal(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	store, schemaName := setupCachedContentStore(t, "cache_isolation_race", 5)
	ctx := context.Background()

	if _, err := store.List(ctx, schemaName, 25, 0, nil); err != nil {
		t.Fatalf("List (priming): %v", err)
	}

	const workers = 8
	const iterations = 40
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)

	decorate := func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			page, err := store.List(ctx, schemaName, 25, 0, nil)
			if err != nil {
				errs <- err
				return
			}
			for _, row := range page {
				row.Data["author"] = map[string]any{"title": "resolved"}
			}
		}
	}

	marshal := func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			page, err := store.List(ctx, schemaName, 25, 0, nil)
			if err != nil {
				errs <- err
				return
			}
			if _, err := json.Marshal(page); err != nil {
				errs <- err
				return
			}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(2)
		go decorate()
		go marshal()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent cached reads: %v", err)
	}
}
