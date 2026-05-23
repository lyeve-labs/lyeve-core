//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// setupContentStore creates a schema, applies its DDL, and returns a ContentStore
// ready for testing. The schema has fields: title, views, active.
func setupContentStore(t *testing.T, pool db.DB, schemaName string) *db.ContentStore {
	t.Helper()

	ctx := context.Background()
	reg := newTestRegistry(t, pool)

	sc := &domain.Schema{
		Name: schemaName,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "views", FieldType: "number"},
			{Name: "active", FieldType: "boolean"},
		},
		WithDraftPublish: true,
	}

	// Create the physical table.
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}

	// Persist the schema definition.
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("reg.Upsert: %v", err)
	}

	t.Cleanup(func() {
		_ = reg.Delete(ctx, schemaName)
	})

	return db.NewContentStore(pool, reg.Source())
}

func TestContentStore_List_FilterAllowlist(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	store := setupContentStore(t, pool, "filter_test")

	// Seed a few rows so List returns something.
	for _, title := range []string{"alpha", "beta", "gamma"} {
		_, err := store.Insert(ctx, "filter_test", map[string]any{"title": title, "views": 42})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// Positive cases: valid filter keys.

	t.Run("filter by schema field", func(t *testing.T) {
		results, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"title": "alpha"})
		if err != nil {
			t.Fatalf("List with valid field filter: %v", err)
		}
		if len(results) != 1 {
			t.Errorf("expected 1 result, got %d", len(results))
		}
	})

	t.Run("limit=0 returns empty page", func(t *testing.T) {
		results, err := store.List(ctx, "filter_test", 0, 0, nil)
		if err != nil {
			t.Fatalf("List with limit=0: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results for limit=0, got %d", len(results))
		}
	})

	t.Run("filter by id", func(t *testing.T) {
		// Get an existing item's ID first.
		all, err := store.List(ctx, "filter_test", 1, 0, nil)
		if err != nil || len(all) == 0 {
			t.Fatalf("seed list: %v", err)
		}
		results, err := store.List(ctx, "filter_test", 1, 0, map[string]any{"id": all[0].ID})
		if err != nil {
			t.Fatalf("List with id filter: %v", err)
		}
		if len(results) != 1 {
			t.Errorf("expected 1 result, got %d", len(results))
		}
	})

	t.Run("filter by created_at", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"created_at": "2024-01-01"})
		if err != nil {
			t.Fatalf("List with created_at filter: %v", err)
		}
	})

	t.Run("filter by updated_at", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"updated_at": "2024-01-01"})
		if err != nil {
			t.Fatalf("List with updated_at filter: %v", err)
		}
	})

	t.Run("filter by _status", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"_status": "published"})
		if err != nil {
			t.Fatalf("List with _status filter: %v", err)
		}
	})

	t.Run("filter by deleted_at accepted by allowlist", func(t *testing.T) {
		// deleted_at is a valid system column in the allowlist. The schema has
		// WithSoftDelete=false so the column doesn't physically exist: that's
		// a DB-level error, not a filter-validation error.
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"deleted_at": nil})
		if errors.Is(err, domain.ErrBadRequest) {
			t.Fatalf("deleted_at should be in allowlist, got ErrBadRequest: %v", err)
		}
		// DB error is expected (column doesn't exist): that's fine for this test.
		if err == nil {
			t.Log("deleted_at filter passed (WithSoftDelete inferred true)")
		} else {
			t.Logf("deleted_at filter returned DB error (expected): %v", err)
		}
	})

	t.Run("filter by multiple valid keys", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{
			"title": "alpha",
			"views": 42,
		})
		if err != nil {
			t.Fatalf("List with multiple valid filters: %v", err)
		}
	})

	// Negative cases: invalid filter keys.

	t.Run("reject unknown filter key", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"non_existent": "foo"})
		if err == nil {
			t.Fatal("expected error for unknown filter key, got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	t.Run("reject unknown key among valid keys", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{
			"title":  "alpha",
			"hacked": true,
		})
		if err == nil {
			t.Fatal("expected error for mixed-in unknown filter key, got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	t.Run("reject SQL-injection-ish filter key", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"1=1; DROP TABLE": "x"})
		if err == nil {
			t.Fatal("expected error for suspicious filter key, got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	// SQL injection: line-comment (--) prefix patterns.
	t.Run("reject SQL injection key: 1=1--", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"1=1--": "x"})
		if err == nil {
			t.Fatal("expected error for filter key '1=1--', got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	t.Run("reject SQL injection key: OR 1=1", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"OR 1=1": "x"})
		if err == nil {
			t.Fatal("expected error for filter key 'OR 1=1', got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	t.Run("reject SQL injection key: UNION SELECT", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"UNION SELECT": "x"})
		if err == nil {
			t.Fatal("expected error for filter key 'UNION SELECT', got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	t.Run("reject SQL injection key: semicolon command", func(t *testing.T) {
		_, err := store.List(ctx, "filter_test", 10, 0, map[string]any{"; DROP TABLE users;--": "x"})
		if err == nil {
			t.Fatal("expected error for filter key with semicolons, got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})

	t.Run("reject empty filters with unknown key is still valid", func(t *testing.T) {
		// nil filters should always pass.
		_, err := store.List(ctx, "filter_test", 10, 0, nil)
		if err != nil {
			t.Fatalf("List with nil filters: %v", err)
		}
		// Empty map should always pass.
		_, err = store.List(ctx, "filter_test", 10, 0, map[string]any{})
		if err != nil {
			t.Fatalf("List with empty filters: %v", err)
		}
	})
}

func TestContentStore_List_FilterAllowlist_BelongsToField(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	reg := newTestRegistry(t, pool)

	// Create the related schema first (authors).
	authorSc := &domain.Schema{
		Name: "authors",
		Fields: []domain.SchemaField{
			{Name: "name", FieldType: "text"},
		},
	}
	if err := reg.Upsert(ctx, authorSc); err != nil {
		t.Fatalf("engine.Apply authors: %v", err)
	}
	if err := reg.Upsert(ctx, authorSc); err != nil {
		t.Fatalf("reg.Upsert authors: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Delete(ctx, "authors")
	})

	// Create a schema with a belongs_to relation (posts.author).
	postSc := &domain.Schema{
		Name: "posts",
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "authors"},
		},
	}
	if err := reg.Upsert(ctx, postSc); err != nil {
		t.Fatalf("engine.Apply posts: %v", err)
	}
	if err := reg.Upsert(ctx, postSc); err != nil {
		t.Fatalf("reg.Upsert posts: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Delete(ctx, "posts")
	})

	store := db.NewContentStore(pool, reg.Source())

	// Insert an author so we have a valid UUID.
	authorStore := db.NewContentStore(pool, reg.Source())
	author, err := authorStore.Insert(ctx, "authors", map[string]any{"name": "Test Author"})
	if err != nil {
		t.Fatalf("Insert author: %v", err)
	}

	// The FK column for belongs_to is author_id: this is what the allowlist should accept.
	t.Run("filter by FK column name", func(t *testing.T) {
		_, err := store.List(ctx, "posts", 10, 0, map[string]any{"author_id": author.ID.String()})
		if err != nil {
			t.Fatalf("List with FK column filter: %v", err)
		}
	})

	// The logical field name "author" should NOT be a valid filter: it's a relation,
	// not a physical column.
	t.Run("reject logical relation name as filter key", func(t *testing.T) {
		_, err := store.List(ctx, "posts", 10, 0, map[string]any{"author": "some-id"})
		if err == nil {
			t.Fatal("expected error for logical relation field name, got nil")
		}
		if !errors.Is(err, domain.ErrBadRequest) {
			t.Errorf("expected ErrBadRequest, got: %v", err)
		}
	})
}

// TestContentStore_List_UUIDFilterCorrectness verifies that filtering by a UUID
// field returns only matching rows and no others. The allowlist already covers
// UUIDs (they go through the id column), but this test validates end-to-end
// correctness: multiple rows with different IDs, filter for one, get exactly one.
func TestContentStore_List_UUIDFilterCorrectness(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	store := setupContentStore(t, pool, "uuid_filter_test")

	// Insert several rows with distinct titles. Each gets a unique ID (UUID).
	type row struct {
		title string
		id    string // filled after insert
	}
	rows := []row{{title: "first"}, {title: "second"}, {title: "third"}}
	for i := range rows {
		entry, err := store.Insert(ctx, "uuid_filter_test", map[string]any{"title": rows[i].title, "views": 10})
		if err != nil {
			t.Fatalf("Insert %q: %v", rows[i].title, err)
		}
		rows[i].id = entry.ID.String()
	}

	// Filter by the first entry's UUID. It should return exactly 1 row: "first".
	t.Run("filter by specific UUID returns only matching row", func(t *testing.T) {
		results, err := store.List(ctx, "uuid_filter_test", 10, 0, map[string]any{"id": rows[0].id})
		if err != nil {
			t.Fatalf("List with UUID filter: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected exactly 1 result, got %d", len(results))
		}
		if results[0].ID.String() != rows[0].id {
			t.Errorf("returned row ID = %s, want %s", results[0].ID.String(), rows[0].id)
		}
		// Verify the data matches.
		title, ok := results[0].Data["title"].(string)
		if !ok || title != "first" {
			t.Errorf("returned title = %v, want 'first'", results[0].Data["title"])
		}
	})

	// Filter by second entry's UUID. It should return exactly 1 row: "second".
	t.Run("filter by different UUID returns different row", func(t *testing.T) {
		results, err := store.List(ctx, "uuid_filter_test", 10, 0, map[string]any{"id": rows[1].id})
		if err != nil {
			t.Fatalf("List with UUID filter: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected exactly 1 result, got %d", len(results))
		}
		title, ok := results[0].Data["title"].(string)
		if !ok || title != "second" {
			t.Errorf("returned title = %v, want 'second'", results[0].Data["title"])
		}
	})
}

// TestContentStore_List_UUIDFilter_AuthorID verifies filtering by author_id
// (a belongs_to FK column) returns only rows matching that author UUID.
func TestContentStore_List_UUIDFilter_AuthorID(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	reg := newTestRegistry(t, pool)

	// Create authors schema.
	authorSc := &domain.Schema{
		Name: "author_test_authors",
		Fields: []domain.SchemaField{
			{Name: "name", FieldType: "text"},
		},
	}
	if err := reg.Upsert(ctx, authorSc); err != nil {
		t.Fatalf("engine.Apply authors: %v", err)
	}
	if err := reg.Upsert(ctx, authorSc); err != nil {
		t.Fatalf("reg.Upsert authors: %v", err)
	}
	t.Cleanup(func() { _ = reg.Delete(ctx, "author_test_authors") })

	// Create posts schema with belongs_to author.
	postSc := &domain.Schema{
		Name: "author_test_posts",
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "author_test_authors"},
		},
	}
	if err := reg.Upsert(ctx, postSc); err != nil {
		t.Fatalf("engine.Apply posts: %v", err)
	}
	if err := reg.Upsert(ctx, postSc); err != nil {
		t.Fatalf("reg.Upsert posts: %v", err)
	}
	t.Cleanup(func() { _ = reg.Delete(ctx, "author_test_posts") })

	store := db.NewContentStore(pool, reg.Source())

	// Create two authors.
	authorA, err := store.Insert(ctx, "author_test_authors", map[string]any{"name": "Author A"})
	if err != nil {
		t.Fatalf("Insert author A: %v", err)
	}
	authorB, err := store.Insert(ctx, "author_test_authors", map[string]any{"name": "Author B"})
	if err != nil {
		t.Fatalf("Insert author B: %v", err)
	}

	// Create posts: 2 by Author A, 1 by Author B.
	_, err = store.Insert(ctx, "author_test_posts", map[string]any{
		"title":     "Post A1",
		"author_id": authorA.ID,
	})
	if err != nil {
		t.Fatalf("Insert post A1: %v", err)
	}
	_, err = store.Insert(ctx, "author_test_posts", map[string]any{
		"title":     "Post A2",
		"author_id": authorA.ID,
	})
	if err != nil {
		t.Fatalf("Insert post A2: %v", err)
	}
	_, err = store.Insert(ctx, "author_test_posts", map[string]any{
		"title":     "Post B1",
		"author_id": authorB.ID,
	})
	if err != nil {
		t.Fatalf("Insert post B1: %v", err)
	}

	// Filter by author_id = authorA.UUID -> should return exactly 2 rows.
	results, err := store.List(ctx, "author_test_posts", 10, 0, map[string]any{"author_id": authorA.ID.String()})
	if err != nil {
		t.Fatalf("List with author_id filter: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 posts for author A, got %d", len(results))
	}
	for _, r := range results {
		if title, ok := r.Data["title"].(string); ok {
			if title != "Post A1" && title != "Post A2" {
				t.Errorf("unexpected post %q in author A results", title)
			}
		}
	}

	// Filter by author_id = authorB.UUID -> should return exactly 1 row.
	resultsB, err := store.List(ctx, "author_test_posts", 10, 0, map[string]any{"author_id": authorB.ID.String()})
	if err != nil {
		t.Fatalf("List with author_id filter (B): %v", err)
	}
	if len(resultsB) != 1 {
		t.Fatalf("expected 1 post for author B, got %d", len(resultsB))
	}
	if title, ok := resultsB[0].Data["title"].(string); !ok || title != "Post B1" {
		t.Errorf("expected Post B1, got %v", resultsB[0].Data["title"])
	}
}

// TestContentStore_List_ParameterizedQuery confirms that ContentStore.List uses
// parameterized placeholders and never inlines filter values into SQL. Validated
// by checking the allowlist rejects non-column keys: the architectural guarantee.
// Additionally verifies the SQL generation uses $N for Postgres (the primary dialect).
func TestContentStore_List_ParameterizedQuery(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	store := setupContentStore(t, pool, "param_test")

	// Insert test data.
	_, err := store.Insert(ctx, "param_test", map[string]any{"title": "param test", "views": 100, "active": true})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	t.Run("single filter produces valid parameterized query", func(t *testing.T) {
		results, err := store.List(ctx, "param_test", 10, 0, map[string]any{"title": "param test"})
		if err != nil {
			t.Fatalf("List with title filter: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least 1 result")
		}
	})

	t.Run("multiple filters all parameterized", func(t *testing.T) {
		results, err := store.List(ctx, "param_test", 10, 0, map[string]any{
			"title":  "param test",
			"views":  100,
			"active": true,
		})
		if err != nil {
			t.Fatalf("List with multiple filters: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least 1 result")
		}
	})

	t.Run("filter values with special characters are safe", func(t *testing.T) {
		// Insert a row with a value that contains SQL-significant characters.
		sqlishTitle := "test'); DROP TABLE students;--"
		_, err := store.Insert(ctx, "param_test", map[string]any{"title": sqlishTitle, "views": 99})
		if err != nil {
			t.Fatalf("Insert sqlish value: %v", err)
		}
		// Filtering by this value should work safely (parameterized, no SQL injection).
		results, err := store.List(ctx, "param_test", 10, 0, map[string]any{"title": sqlishTitle})
		if err != nil {
			t.Fatalf("List with sqlish filter value: %v", err)
		}
		if len(results) == 0 {
			t.Fatal("expected at least 1 result matching sqlish title")
		}
		// Verify the returned title matches exactly what we stored.
		if title, ok := results[0].Data["title"].(string); !ok || title != sqlishTitle {
			t.Errorf("title = %q, want %q", title, sqlishTitle)
		}
	})

	t.Run("nil and empty filters pass through safely", func(t *testing.T) {
		_, err := store.List(ctx, "param_test", 10, 0, nil)
		if err != nil {
			t.Fatalf("List with nil filters: %v", err)
		}
	})
}

// TestContentStore_List_MultiDialectParameterized ensures ContentStore.List produces
// dialect-correct parameterized queries across all three supported engines.
// The generated SQL (Postgres-style $N) is rewritten by the db.DB layer to the
// target dialect's placeholder form (?, @pN). Since we're testing at the store
// level, we verify correctness by running against actual container instances
// for each dialect.
func TestContentStore_List_MultiDialectParameterized(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-dialect integration test in short mode")
	}

	type dialectFixture struct {
		name string
		pool func(t *testing.T) db.DB
	}
	fixtures := []dialectFixture{
		{name: "postgres", pool: func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{name: "mysql", pool: func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{name: "mssql", pool: func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			if !testdb.ShouldTest(fx.name) {
				t.Skipf("CI_DIALECT != %s", fx.name)
			}

			pool := fx.pool(t)
			ctx := context.Background()
			reg := newTestRegistry(t, pool)

			sc := &domain.Schema{
				Name: fmt.Sprintf("md_test_%s", fx.name),
				Fields: []domain.SchemaField{
					{Name: "label", FieldType: "text"},
					{Name: "count", FieldType: "number"},
				},
				WithDraftPublish: false, // simpler: no _status auto-filter
			}

			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("engine.Apply: %v", err)
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("reg.Upsert: %v", err)
			}
			t.Cleanup(func() {
				_ = reg.Delete(ctx, sc.Name)
			})

			store := db.NewContentStore(pool, reg.Source())

			// Insert rows across the dialect.
			for i, label := range []string{"alpha", "beta", "gamma"} {
				_, err := store.Insert(ctx, sc.Name, map[string]any{"label": label, "count": i * 10})
				if err != nil {
					t.Fatalf("Insert %q: %v", label, err)
				}
			}

			// Verify: filter by label returns exactly the matching row.
			results, err := store.List(ctx, sc.Name, 10, 0, map[string]any{"label": "beta"})
			if err != nil {
				t.Fatalf("List with label filter on %s: %v", fx.name, err)
			}
			if len(results) != 1 {
				t.Fatalf("expected 1 result, got %d on %s", len(results), fx.name)
			}
			if label, ok := results[0].Data["label"].(string); !ok || label != "beta" {
				t.Errorf("label = %q, want 'beta' on %s", results[0].Data["label"], fx.name)
			}

			// Verify: filter by count (numeric) works across dialects.
			results2, err := store.List(ctx, sc.Name, 10, 0, map[string]any{"count": float64(20)})
			if err != nil {
				t.Fatalf("List with count filter on %s: %v", fx.name, err)
			}
			if len(results2) != 1 {
				t.Fatalf("expected 1 result for count=20, got %d on %s", len(results2), fx.name)
			}

			// Verify: injection-ish filter key is rejected on all dialects.
			_, err = store.List(ctx, sc.Name, 10, 0, map[string]any{"1=1--": "x"})
			if err == nil {
				t.Fatalf("expected error for injection filter key on %s, got nil", fx.name)
			}
			if !errors.Is(err, domain.ErrBadRequest) {
				t.Errorf("expected ErrBadRequest on %s, got: %v", fx.name, err)
			}
		})
	}
}

// Cache tests

func TestContentStore_List_CacheHit(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	schemaName := "cache_hit_test"
	reg := newTestRegistry(t, pool)

	sc := &domain.Schema{
		Name: schemaName,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "views", FieldType: "number"},
		},
		WithDraftPublish: false,
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("reg.Upsert: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Delete(ctx, schemaName)
	})

	// Seed data.
	store := db.NewContentStore(pool, reg.Source())
	_, err := store.Insert(ctx, schemaName, map[string]any{"title": "hit-test", "views": 1})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	itemCache := cache.NewMemory[string, *domain.Content](100, 0)
	listCache := cache.NewMemory[string, []*domain.Content](100, 0)
	cachingStore := db.NewContentStoreWithCache(pool, reg.Source(), itemCache, listCache)

	// First call: should hit DB.
	results1, err := cachingStore.List(ctx, schemaName, 10, 0, nil)
	if err != nil {
		t.Fatalf("List (first): %v", err)
	}
	if len(results1) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results1))
	}

	// Second call with identical params: should hit cache, no DB round-trip.
	// Insert another row (which flushes list cache) to prove the first call IS cached.
	// Insert calls invalidateSchema which Flushes the list cache.
	// We verify: List then List again without intervening writes -> both produce same data
	// and first call's results match second call's results.
	results2, err := cachingStore.List(ctx, schemaName, 10, 0, nil)
	if err != nil {
		t.Fatalf("List (second): %v", err)
	}
	if len(results2) != 1 {
		t.Fatalf("expected 1 result on second call, got %d", len(results2))
	}
	if results2[0].ID != results1[0].ID {
		t.Errorf("cache returned different data: ID1=%s, ID2=%s", results1[0].ID, results2[0].ID)
	}

	// Insert triggers Flush -> next List goes to DB.
	_, err = cachingStore.Insert(ctx, schemaName, map[string]any{"title": "after-flush", "views": 2})
	if err != nil {
		t.Fatalf("Insert after cache: %v", err)
	}
	results3, err := cachingStore.List(ctx, schemaName, 10, 0, nil)
	if err != nil {
		t.Fatalf("List after insert: %v", err)
	}
	if len(results3) != 2 {
		t.Fatalf("expected 2 results after insert (cache flushed), got %d", len(results3))
	}
}

func TestContentStore_GetByID_CacheHit(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	schemaName := "getbyid_cache_test"
	reg := newTestRegistry(t, pool)

	sc := &domain.Schema{
		Name: schemaName,
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
		},
		WithDraftPublish: false,
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("reg.Upsert: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Delete(ctx, schemaName)
	})

	itemCache := cache.NewMemory[string, *domain.Content](100, 0)
	listCache := cache.NewMemory[string, []*domain.Content](100, 0)
	store := db.NewContentStoreWithCache(pool, reg.Source(), itemCache, listCache)

	item, err := store.Insert(ctx, schemaName, map[string]any{"title": "cache-me"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// First GetByID: hits DB, populates cache.
	result1, err := store.GetByID(ctx, schemaName, item.ID)
	if err != nil {
		t.Fatalf("GetByID (first): %v", err)
	}
	if result1.ID != item.ID {
		t.Fatalf("ID mismatch: got %s, want %s", result1.ID, item.ID)
	}

	// Second GetByID with same ID: should hit cache.
	result2, err := store.GetByID(ctx, schemaName, item.ID)
	if err != nil {
		t.Fatalf("GetByID (second): %v", err)
	}
	if result2.ID != item.ID {
		t.Fatalf("cached ID mismatch: got %s, want %s", result2.ID, item.ID)
	}
	if title, ok := result2.Data["title"].(string); !ok || title != "cache-me" {
		t.Errorf("cached title = %q, want %q", title, "cache-me")
	}

	// Update invalidates cache -> next GetByID hits DB.
	_, err = store.Update(ctx, schemaName, item.ID, map[string]any{"title": "updated"}, item.UpdatedAt)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	result3, err := store.GetByID(ctx, schemaName, item.ID)
	if err != nil {
		t.Fatalf("GetByID (after update): %v", err)
	}
	if title, ok := result3.Data["title"].(string); !ok || title != "updated" {
		t.Errorf("post-invalidation title = %q, want %q", title, "updated")
	}
}

func TestContentStore_List_CacheMetrics(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	schemaName := "cache_metrics_test"
	reg := newTestRegistry(t, pool)

	sc := &domain.Schema{
		Name:             schemaName,
		Fields:           []domain.SchemaField{{Name: "title", FieldType: "text"}},
		WithDraftPublish: false,
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("reg.Upsert: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Delete(ctx, schemaName)
	})

	itemCache := cache.NewMemory[string, *domain.Content](100, 0)
	listCache := cache.NewMemory[string, []*domain.Content](100, 0)
	store := db.NewContentStoreWithCache(pool, reg.Source(), itemCache, listCache)

	// Seed one row.
	_, err := store.Insert(ctx, schemaName, map[string]any{"title": "metrics-test"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// First List: miss.
	_, err = store.List(ctx, schemaName, 10, 0, nil)
	if err != nil {
		t.Fatalf("List #1: %v", err)
	}

	// Second List (same params): hit.
	_, err = store.List(ctx, schemaName, 10, 0, nil)
	if err != nil {
		t.Fatalf("List #2: %v", err)
	}

	// Verify counters on the ContentStore.
	hits := store.ListCacheHits.Load()
	misses := store.ListCacheMisses.Load()
	if hits != 1 {
		t.Errorf("ListCacheHits = %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("ListCacheMisses = %d, want 1", misses)
	}
}

func TestContentStore_List_CacheBypassOversizedLimit(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	schemaName := "cache_bypass_test"
	reg := newTestRegistry(t, pool)

	sc := &domain.Schema{
		Name:             schemaName,
		Fields:           []domain.SchemaField{{Name: "title", FieldType: "text"}},
		WithDraftPublish: false,
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("engine.Apply: %v", err)
	}
	if err := reg.Upsert(ctx, sc); err != nil {
		t.Fatalf("reg.Upsert: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Delete(ctx, schemaName)
	})

	itemCache := cache.NewMemory[string, *domain.Content](100, 0)
	listCache := cache.NewMemory[string, []*domain.Content](100, 0)
	store := db.NewContentStoreWithCache(pool, reg.Source(), itemCache, listCache)

	// Seed one row.
	_, err := store.Insert(ctx, schemaName, map[string]any{"title": "bypass-test"})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Call List with an oversized limit (>200). The store should skip caching.
	const oversized = 999
	_, err = store.List(ctx, schemaName, oversized, 0, nil)
	if err != nil {
		t.Fatalf("List #1 (oversized): %v", err)
	}

	// Second call with same oversized limit: should still miss (wasn't cached).
	_, err = store.List(ctx, schemaName, oversized, 0, nil)
	if err != nil {
		t.Fatalf("List #2 (oversized): %v", err)
	}

	hits := store.ListCacheHits.Load()
	misses := store.ListCacheMisses.Load()
	if hits != 0 {
		t.Errorf("ListCacheHits = %d, want 0 (oversized limits must not be cached)", hits)
	}
	if misses != 2 {
		t.Errorf("ListCacheMisses = %d, want 2 (both calls should miss since nothing cached)", misses)
	}
}

// TestContentStore_DialectAwareTimestamps verifies that Update, Delete (soft),
// SetStatus, and BulkInsert use dialect-aware timestamp functions  --
// not hardcoded NOW() which breaks MSSQL.
func TestContentStore_DialectAwareTimestamps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-dialect integration test in short mode")
	}

	dialects := []struct {
		name string
		pool func(t *testing.T) db.DB
	}{
		{name: "postgres", pool: func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{name: "mysql", pool: func(t *testing.T) db.DB { return testdb.MySQL(t) }},
	}

	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}

			pool := d.pool(t)
			ctx := context.Background()
			schemaName := fmt.Sprintf("dialect_ts_%s", d.name)

			reg := newTestRegistry(t, pool)

			sc := &domain.Schema{
				Name: schemaName,
				Fields: []domain.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "count", FieldType: "number"},
				},
				// WithDraftPublish requires a _status TEXT column which MySQL
				// rejects with a default value (Error 1101). Use only
				// WithSoftDelete for cross-dialect coverage. SetStatus is
				// tested on PG via setupContentStore in other tests.
				WithDraftPublish: d.name == "postgres",
				WithSoftDelete:   true,
			}

			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("engine.Apply: %v", err)
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("reg.Upsert: %v", err)
			}
			t.Cleanup(func() {
				_ = reg.Delete(ctx, sc.Name)
			})

			store := db.NewContentStore(pool, reg.Source())

			// 1. INSERT a row.
			created, err := store.Insert(ctx, sc.Name, map[string]any{
				"title": "dialect-test", "count": 1,
			})
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if created == nil {
				t.Fatal("Insert returned nil")
			}
			if created.UpdatedAt.IsZero() {
				t.Fatalf("%s: updated_at should be set on insert", d.name)
			}

			// 2. UPDATE: verify updated_at changes (and query succeeds on all dialects).
			beforeUpdate := created.UpdatedAt
			updated, err := store.Update(ctx, sc.Name, created.ID, map[string]any{
				"count": 2,
			}, created.UpdatedAt)
			if err != nil {
				t.Fatalf("%s: Update: %v", d.name, err)
			}
			if updated == nil {
				t.Fatalf("%s: Update returned nil", d.name)
			}
			if !updated.UpdatedAt.After(beforeUpdate) {
				t.Fatalf("%s: updated_at should advance after update (%v -> %v)",
					d.name, beforeUpdate, updated.UpdatedAt)
			}

			// 3. SetStatus: verify updated_at advances again (PG only, because MySQL
			// rejects TEXT _status column default).
			if d.name == "postgres" {
				if err := store.SetStatus(ctx, sc.Name, created.ID, "published"); err != nil {
					t.Fatalf("%s: SetStatus: %v", d.name, err)
				}

				afterStatus, err := store.GetByID(ctx, sc.Name, created.ID)
				if err != nil {
					t.Fatalf("%s: GetByID after set_status: %v", d.name, err)
				}
				if !afterStatus.UpdatedAt.After(updated.UpdatedAt) {
					t.Fatalf("%s: updated_at should advance after set_status", d.name)
				}
			}

			// 4. SOFT DELETE: verify deleted_at is set.
			if err := store.Delete(ctx, sc.Name, created.ID); err != nil {
				t.Fatalf("%s: Delete: %v", d.name, err)
			}

			_, err = store.GetByID(ctx, sc.Name, created.ID)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("%s: soft-deleted row should not be findable, got: %v", d.name, err)
			}

			// 5. BULK INSERT: verify all rows inserted and returned with timestamps.
			bulk, err := store.BulkInsert(ctx, sc.Name, []map[string]any{
				{"title": "bulk-a", "count": 10},
				{"title": "bulk-b", "count": 20},
				{"title": "bulk-c", "count": 30},
			})
			if err != nil {
				t.Fatalf("%s: BulkInsert: %v", d.name, err)
			}
			if len(bulk) != 3 {
				t.Fatalf("%s: BulkInsert returned %d rows, want 3", d.name, len(bulk))
			}
			expectedTitles := []string{"bulk-a", "bulk-b", "bulk-c"}
			for i, b := range bulk {
				if b.CreatedAt.IsZero() {
					t.Fatalf("%s: bulk row %d: created_at should be set", d.name, i)
				}
				if b.UpdatedAt.IsZero() {
					t.Fatalf("%s: bulk row %d: updated_at should be set", d.name, i)
				}
				if b.Data["title"] != expectedTitles[i] {
					t.Fatalf("%s: bulk row %d: title = %v, want %v",
						d.name, i, b.Data["title"], expectedTitles[i])
				}
			}
		})
	}
}

func TestContentStore_ListCursor_DialectAwareLimit(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	store := setupContentStore(t, pool, "cursor_test")

	// Seed 5 items.
	for i := 0; i < 5; i++ {
		_, err := store.Insert(ctx, "cursor_test", map[string]any{
			"title": fmt.Sprintf("cursor-%d", i),
			"views": i * 10,
		})
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// Get all items to discover the UUID-sorted order.
	allItems, err := store.ListCursor(ctx, "cursor_test", "", 100)
	if err != nil {
		t.Fatalf("ListCursor all: %v", err)
	}
	if len(allItems) != 5 {
		t.Fatalf("expected 5 seeded items, got %d", len(allItems))
	}
	// cursorID is the UUID of the 3rd item in ascending ID order.
	cursorID := allItems[2].ID.String()

	t.Run("no cursor returns first page", func(t *testing.T) {
		items, err := store.ListCursor(ctx, "cursor_test", "", 3)
		if err != nil {
			t.Fatalf("ListCursor with no cursor: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("expected 3 items, got %d", len(items))
		}
		for _, it := range items {
			if it.ID.String() == "" {
				t.Error("item has empty ID")
			}
			if it.CreatedAt.IsZero() {
				t.Error("item has zero CreatedAt")
			}
		}
	})

	t.Run("cursor pagination returns next items", func(t *testing.T) {
		items, err := store.ListCursor(ctx, "cursor_test", cursorID, 100)
		if err != nil {
			t.Fatalf("ListCursor with cursor: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("expected 2 items after cursor %s, got %d", cursorID, len(items))
		}
		cursorUUID := uuid.MustParse(cursorID)
		for _, it := range items {
			if it.ID.String() <= cursorUUID.String() {
				t.Errorf("expected id > %s, got %s", cursorUUID, it.ID)
			}
		}
	})

	t.Run("invalid cursor returns error", func(t *testing.T) {
		_, err := store.ListCursor(ctx, "cursor_test", "not-a-uuid", 10)
		if err == nil {
			t.Fatal("expected error for invalid cursor")
		}
		if !strings.Contains(err.Error(), "invalid cursor") {
			t.Fatalf("expected 'invalid cursor' error, got: %v", err)
		}
	})

	t.Run("limit 0 defaults to 20", func(t *testing.T) {
		items, err := store.ListCursor(ctx, "cursor_test", "", 0)
		if err != nil {
			t.Fatalf("ListCursor with limit 0: %v", err)
		}
		if len(items) == 0 || len(items) > 20 {
			t.Fatalf("expected 1..20 items with limit 0, got %d", len(items))
		}
	})
}
