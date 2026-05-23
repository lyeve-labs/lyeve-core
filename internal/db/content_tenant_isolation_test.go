//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// TestContentTenantIsolation_SharedSchemaNameIsolatedRows proves that two
// tenants defining the same content type share one physical table but see
// only their own rows: a write by tenant A is invisible to tenant B for
// every read path, and B cannot update, delete, or publish A's rows.
func TestContentTenantIsolation_SharedSchemaNameIsolatedRows(t *testing.T) {
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

			slugA := fmt.Sprintf("ct_a_%d", time.Now().UnixNano()%100000)
			slugB := fmt.Sprintf("ct_b_%d", time.Now().UnixNano()%100000)
			for _, s := range []string{slugA, slugB} {
				createTenant(t, ctx, pool, s)
			}
			t.Cleanup(func() {
				dropTenant(t, pool, slugA)
				dropTenant(t, pool, slugB)
			})

			baseA, connA := acquireTenant(t, ctx, pool, slugA)
			defer connA.Close()
			baseB, connB := acquireTenant(t, ctx, pool, slugB)
			defer connB.Close()
			ctxA := tenant.WithID(baseA, slugA)
			ctxB := tenant.WithID(baseB, slugB)

			reg := newTestRegistry(t, pool)
			sc := &domain.Schema{
				Name: "articles",
				Fields: []domain.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "views", FieldType: "number"},
				},
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("engine.Apply: %v", err)
			}
			if err := reg.Upsert(ctxA, sc); err != nil {
				t.Fatalf("upsert schema A: %v", err)
			}
			if err := reg.Upsert(ctxB, sc); err != nil {
				t.Fatalf("upsert schema B: %v", err)
			}

			store := db.NewContentStore(pool, reg.Source())

			a, err := store.Insert(ctxA, "articles", map[string]any{"title": "alpha", "views": 1})
			if err != nil {
				t.Fatalf("insert A: %v", err)
			}
			b, err := store.Insert(ctxB, "articles", map[string]any{"title": "beta", "views": 2})
			if err != nil {
				t.Fatalf("insert B: %v", err)
			}

			titles := func(items []*domain.Content) map[string]bool {
				out := make(map[string]bool, len(items))
				for _, it := range items {
					out[fmt.Sprint(it.Data["title"])] = true
				}
				return out
			}

			// List: each tenant sees only its own row.
			listA, err := store.List(ctxA, "articles", 20, 0, nil)
			if err != nil {
				t.Fatalf("list A: %v", err)
			}
			if got := titles(listA); !got["alpha"] || got["beta"] || len(got) != 1 {
				t.Fatalf("tenant A list leaked rows: %v", got)
			}
			listB, err := store.List(ctxB, "articles", 20, 0, nil)
			if err != nil {
				t.Fatalf("list B: %v", err)
			}
			if got := titles(listB); !got["beta"] || got["alpha"] || len(got) != 1 {
				t.Fatalf("tenant B list leaked rows: %v", got)
			}

			// Filtered list: a tenant filter must not widen the scope.
			byTitleA, err := store.List(ctxA, "articles", 20, 0, map[string]any{"title": "beta"})
			if err != nil {
				t.Fatalf("list A filtered: %v", err)
			}
			if len(byTitleA) != 0 {
				t.Fatalf("tenant A filtered list leaked B's row: %d rows", len(byTitleA))
			}

			// GetByID: cross-tenant read returns ErrNotFound.
			if _, err := store.GetByID(ctxB, "articles", a.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("cross-tenant get: want ErrNotFound, got %v", err)
			}

			// Update: cross-tenant update fails as a conflict and changes nothing.
			if _, err := store.Update(ctxB, "articles", a.ID, map[string]any{"title": "hijacked"}, time.Time{}); !errors.Is(err, db.ErrContentConflict) {
				t.Fatalf("cross-tenant update: want ErrContentConflict, got %v", err)
			}
			after, err := store.GetByID(ctxA, "articles", a.ID)
			if err != nil {
				t.Fatalf("get A after cross-tenant update: %v", err)
			}
			if after.Data["title"] != "alpha" {
				t.Fatalf("cross-tenant update mutated A's row: %v", after.Data)
			}

			// Delete: cross-tenant delete returns ErrNotFound and keeps the row.
			if err := store.Delete(ctxB, "articles", a.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("cross-tenant delete: want ErrNotFound, got %v", err)
			}
			if _, err := store.GetByID(ctxA, "articles", a.ID); err != nil {
				t.Fatalf("A's row gone after cross-tenant delete: %v", err)
			}

			// BulkInsert from B stays invisible to A.
			if _, err := store.BulkInsert(ctxB, "articles", []map[string]any{{"title": "gamma"}}); err != nil {
				t.Fatalf("bulk insert B: %v", err)
			}
			listA, err = store.List(ctxA, "articles", 20, 0, nil)
			if err != nil {
				t.Fatalf("list A after bulk: %v", err)
			}
			if got := titles(listA); got["gamma"] || len(got) != 1 {
				t.Fatalf("tenant A list leaked B's bulk row: %v", got)
			}

			// Cursor pagination: scoped per tenant.
			cursorA, err := store.ListCursor(ctxA, "articles", "", 20)
			if err != nil {
				t.Fatalf("cursor list A: %v", err)
			}
			if got := titles(cursorA); got["beta"] || got["gamma"] || len(got) != 1 {
				t.Fatalf("tenant A cursor list leaked rows: %v", got)
			}

			// A deletes its own row. B's rows stay untouched.
			if err := store.Delete(ctxA, "articles", a.ID); err != nil {
				t.Fatalf("delete own row A: %v", err)
			}
			if _, err := store.GetByID(ctxB, "articles", b.ID); err != nil {
				t.Fatalf("B's row gone after A's delete: %v", err)
			}

			// SetStatus: cross-tenant publish returns ErrNotFound. Draft/publish
			// schemas exercise the same tenant filter through the _status path.
			if d.name == "postgres" {
				draft := &domain.Schema{
					Name: "drafts",
					Fields: []domain.SchemaField{
						{Name: "title", FieldType: "text"},
					},
					WithDraftPublish: true,
				}
				if err := reg.Upsert(ctx, draft); err != nil {
					t.Fatalf("engine.Apply drafts: %v", err)
				}
				if err := reg.Upsert(ctxA, draft); err != nil {
					t.Fatalf("upsert drafts A: %v", err)
				}
				if err := reg.Upsert(ctxB, draft); err != nil {
					t.Fatalf("upsert drafts B: %v", err)
				}
				draftA, err := store.Insert(ctxA, "drafts", map[string]any{"title": "draft-a"})
				if err != nil {
					t.Fatalf("insert draft A: %v", err)
				}
				if err := store.SetStatus(ctxB, "drafts", draftA.ID, "draft"); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("cross-tenant set status: want ErrNotFound, got %v", err)
				}
				if err := store.SetStatus(ctxA, "drafts", draftA.ID, "draft"); err != nil {
					t.Fatalf("own set status: %v", err)
				}
			}
		})
	}
}

// TestContentTenantIsolation_PivotRowsScopedToTenant proves that
// many_to_many pivot rows are scoped to the tenant that wrote them: tenant B
// cannot read, destroy, or inflate tenant A's relations.
func TestContentTenantIsolation_PivotRowsScopedToTenant(t *testing.T) {
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

			slugA := fmt.Sprintf("ctp_a_%d", time.Now().UnixNano()%100000)
			slugB := fmt.Sprintf("ctp_b_%d", time.Now().UnixNano()%100000)
			for _, s := range []string{slugA, slugB} {
				createTenant(t, ctx, pool, s)
			}
			t.Cleanup(func() {
				dropTenant(t, pool, slugA)
				dropTenant(t, pool, slugB)
			})

			baseA, connA := acquireTenant(t, ctx, pool, slugA)
			defer connA.Close()
			baseB, connB := acquireTenant(t, ctx, pool, slugB)
			defer connB.Close()
			ctxA := tenant.WithID(baseA, slugA)
			ctxB := tenant.WithID(baseB, slugB)

			reg := newTestRegistry(t, pool)
			articles := &domain.Schema{
				Name: "articles",
				Fields: []domain.SchemaField{
					{Name: "title", FieldType: "text"},
					{Name: "tags", FieldType: "relation", RelationTo: "tags", RelationType: domain.RelManyToMany},
				},
			}
			tags := &domain.Schema{
				Name:   "tags",
				Fields: []domain.SchemaField{{Name: "label", FieldType: "text"}},
			}
			// tags first: the pivot table on articles references _tags.
			if err := reg.Upsert(ctx, tags); err != nil {
				t.Fatalf("engine.Apply tags: %v", err)
			}
			if err := reg.Upsert(ctx, articles); err != nil {
				t.Fatalf("engine.Apply articles: %v", err)
			}
			for _, sc := range []*domain.Schema{articles, tags} {
				if err := reg.Upsert(ctxA, sc); err != nil {
					t.Fatalf("upsert schema A %s: %v", sc.Name, err)
				}
				if err := reg.Upsert(ctxB, sc); err != nil {
					t.Fatalf("upsert schema B %s: %v", sc.Name, err)
				}
			}

			store := db.NewContentStore(pool, reg.Source())
			tagsField := articles.Fields[1]

			articleA, err := store.Insert(ctxA, "articles", map[string]any{"title": "a-post"})
			if err != nil {
				t.Fatalf("insert article A: %v", err)
			}
			articleB, err := store.Insert(ctxB, "articles", map[string]any{"title": "b-post"})
			if err != nil {
				t.Fatalf("insert article B: %v", err)
			}
			tagA, err := store.Insert(ctxA, "tags", map[string]any{"label": "a-tag"})
			if err != nil {
				t.Fatalf("insert tag A: %v", err)
			}
			tagB, err := store.Insert(ctxB, "tags", map[string]any{"label": "b-tag"})
			if err != nil {
				t.Fatalf("insert tag B: %v", err)
			}

			if err := store.SetRelations(ctxA, "articles", articleA.ID, tagsField, []uuid.UUID{tagA.ID}); err != nil {
				t.Fatalf("set relations A: %v", err)
			}

			// A sees its relation. B probing A's article id sees none and no
			// inflated count.
			relA, totalA, err := store.ListRelated(ctxA, "articles", articleA.ID, tagsField, 20, 0)
			if err != nil {
				t.Fatalf("list related A: %v", err)
			}
			if len(relA) != 1 || relA[0].Data["label"] != "a-tag" || totalA != 1 {
				t.Fatalf("tenant A relations wrong: rows=%d total=%d", len(relA), totalA)
			}
			relB, totalB, err := store.ListRelated(ctxB, "articles", articleA.ID, tagsField, 20, 0)
			if err != nil {
				t.Fatalf("list related B: %v", err)
			}
			if len(relB) != 0 || totalB != 0 {
				t.Fatalf("tenant B saw A's relations: rows=%d total=%d", len(relB), totalB)
			}

			// B writing relations onto A's article id must not destroy A's rows.
			if err := store.SetRelations(ctxB, "articles", articleA.ID, tagsField, []uuid.UUID{tagB.ID}); err != nil {
				t.Fatalf("set relations B: %v", err)
			}
			relA, _, err = store.ListRelated(ctxA, "articles", articleA.ID, tagsField, 20, 0)
			if err != nil {
				t.Fatalf("list related A after B write: %v", err)
			}
			if len(relA) != 1 || relA[0].Data["label"] != "a-tag" {
				t.Fatalf("tenant B's pivot write destroyed A's relations: %v", relA)
			}

			// Batch resolution through populate stays tenant-scoped too.
			items, err := store.List(ctxB, "articles", 20, 0, nil)
			if err != nil {
				t.Fatalf("list articles B: %v", err)
			}
			if len(items) != 1 || items[0].ID != articleB.ID {
				t.Fatalf("tenant B articles wrong: %v", items)
			}
		})
	}
}
