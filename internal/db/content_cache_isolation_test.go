package db_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// setupCachedContentStore returns a store backed by real in-memory caches, with
// rows already seeded, plus the schema name.
func setupCachedContentStore(t *testing.T, schemaName string, rows int) (*db.ContentStore, string) {
	t.Helper()
	return cachedContentStoreOn(t, testdb.Postgres(t), schemaName, rows), schemaName
}

// cachedContentStoreOn is setupCachedContentStore on a pool of any dialect.
func cachedContentStoreOn(t *testing.T, pool db.DB, schemaName string, rows int) *db.ContentStore {
	t.Helper()
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
	return db.NewContentStoreWithCache(pool, reg.Source(), itemCache, listCache)
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

// GetByIDs is the batch read relation population makes, and it shares the item
// cache with GetByID. The rows it returns are the caller's to decorate, on a
// miss, which is the read that fills the cache, and on a hit alike.
func TestContentStore_CachedGetByIDs_DoesNotShareDataWithCallers(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		store := cachedContentStoreOn(t, pool, "cache_isolation_batch", 2)
		ctx := context.Background()

		page, err := store.List(ctx, "cache_isolation_batch", 25, 0, nil)
		if err != nil || len(page) != 2 {
			t.Fatalf("seed list: %d rows, %v", len(page), err)
		}
		ids := []uuid.UUID{page[0].ID, page[1].ID}

		decorate := func(t *testing.T, rows map[uuid.UUID]*domain.Content, by string) {
			t.Helper()
			if len(rows) != len(ids) {
				t.Fatalf("GetByIDs answered %d rows, want %d", len(rows), len(ids))
			}
			for _, row := range rows {
				row.Data["author"] = map[string]any{"title": "resolved by " + by}
				nested, ok := row.Data["attributes"].(map[string]any)
				if !ok {
					t.Fatalf("attributes did not decode as a map, so the fixture does not exercise nesting: %T", row.Data["attributes"])
				}
				nested["injected"] = by
			}
		}
		untouched := func(t *testing.T, row *domain.Content, read string) {
			t.Helper()
			if v, leaked := row.Data["author"]; leaked {
				t.Errorf("%s: a relation written by an earlier caller reached this one through the item cache: %v", read, v)
			}
			if nested, ok := row.Data["attributes"].(map[string]any); ok {
				if v, leaked := nested["injected"]; leaked {
					t.Errorf("%s: a nested value written by an earlier caller reached this one through the item cache: %v", read, v)
				}
			}
		}

		missed, err := store.GetByIDs(ctx, "cache_isolation_batch", ids)
		if err != nil {
			t.Fatalf("GetByIDs (miss): %v", err)
		}
		decorate(t, missed, "the miss")

		hit, err := store.GetByIDs(ctx, "cache_isolation_batch", ids)
		if err != nil {
			t.Fatalf("GetByIDs (hit): %v", err)
		}
		for _, id := range ids {
			untouched(t, hit[id], "GetByIDs after a miss")
		}
		decorate(t, hit, "a hit")

		for _, id := range ids {
			again, err := store.GetByIDs(ctx, "cache_isolation_batch", []uuid.UUID{id})
			if err != nil {
				t.Fatalf("GetByIDs (second hit): %v", err)
			}
			untouched(t, again[id], "GetByIDs after a hit")

			one, err := store.GetByID(ctx, "cache_isolation_batch", id)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			untouched(t, one, "GetByID")
		}
	})
}

// Population walks into the related rows GetByIDs read and fills in their own
// relations. What it fills in belongs to the caller that asked, so the cached
// row stays as stored for a later read, populated or not. The walk runs twice
// because the first reads the author on a miss and the second on a hit.
func TestContentStore_CachedPopulate_LeavesCachedRowsAsStored(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg := newTestRegistry(t, pool)
		for _, sc := range []*domain.Schema{
			{Name: "cpop_agent", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}},
			{Name: "cpop_author", Fields: []domain.SchemaField{
				{Name: "name", FieldType: "text"},
				{Name: "agent", FieldType: "relation", RelationTo: "cpop_agent", RelationType: domain.RelBelongsTo},
			}},
			{Name: "cpop_post", Fields: []domain.SchemaField{
				{Name: "title", FieldType: "text"},
				{Name: "author", FieldType: "relation", RelationTo: "cpop_author", RelationType: domain.RelBelongsTo},
			}},
		} {
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("Upsert %s: %v", sc.Name, err)
			}
		}
		t.Cleanup(func() {
			for _, name := range []string{"cpop_post", "cpop_author", "cpop_agent"} {
				_ = reg.Delete(context.Background(), name)
			}
		})

		store := db.NewContentStoreWithCache(pool, reg.Source(),
			cache.NewMemory[string, *domain.Content](100, 0), cache.NewMemory[string, []*domain.Content](100, 0))
		agent, err := store.Insert(ctx, "cpop_agent", map[string]any{"name": "smith"})
		if err != nil {
			t.Fatalf("Insert agent: %v", err)
		}
		author, err := store.Insert(ctx, "cpop_author", map[string]any{"name": "ada", "agent": agent.ID.String()})
		if err != nil {
			t.Fatalf("Insert author: %v", err)
		}
		post, err := store.Insert(ctx, "cpop_post", map[string]any{"title": "hello", "author": author.ID.String()})
		if err != nil {
			t.Fatalf("Insert post: %v", err)
		}

		for _, read := range []string{"miss", "hit"} {
			p, err := store.GetByID(ctx, "cpop_post", post.ID)
			if err != nil {
				t.Fatalf("%s: GetByID post: %v", read, err)
			}
			if err := store.PopulateWithConfig(ctx, p, domain.PopulateConfig{Paths: []string{"author.agent"}}); err != nil {
				t.Fatalf("%s: populate: %v", read, err)
			}
			populatedAuthor, ok := p.Data["author"].(map[string]any)
			if !ok {
				t.Fatalf("%s: the author was not populated: %v", read, p.Data["author"])
			}
			if _, ok := populatedAuthor["agent"].(map[string]any); !ok {
				t.Fatalf("%s: the second hop was not populated: %v", read, populatedAuthor["agent"])
			}

			one, err := store.GetByID(ctx, "cpop_author", author.ID)
			if err != nil {
				t.Fatalf("%s: GetByID author: %v", read, err)
			}
			if v, leaked := one.Data["agent"].(map[string]any); leaked {
				t.Errorf("%s: GetByID served the author with the agent a populate wrote into the cached row: %v", read, v)
			}
			batch, err := store.GetByIDs(ctx, "cpop_author", []uuid.UUID{author.ID})
			if err != nil {
				t.Fatalf("%s: GetByIDs author: %v", read, err)
			}
			if v, leaked := batch[author.ID].Data["agent"].(map[string]any); leaked {
				t.Errorf("%s: GetByIDs served the author with the agent a populate wrote into the cached row: %v", read, v)
			}
		}
	})
}
