package db_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// eachDialect runs fn against every enabled dialect pool, so the store
// behavior below (roles array encoding, pivot-row replacement) is exercised on
// real MySQL and MSSQL, not only Postgres.
func eachDialect(t *testing.T, fn func(t *testing.T, pool db.DB)) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping multi-dialect integration test in short mode")
	}
	fixtures := []struct {
		name string
		open func(t *testing.T) db.DB
	}{
		{"postgres", func(t *testing.T) db.DB { return testdb.Postgres(t) }},
		{"mysql", func(t *testing.T) db.DB { return testdb.MySQL(t) }},
		{"mssql", func(t *testing.T) db.DB { return testdb.MSSQL(t) }},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			if !testdb.ShouldTest(fx.name) {
				t.Skipf("CI_DIALECT != %s", fx.name)
			}
			fn(t, fx.open(t))
		})
	}
}

// roles []string must round-trip through Create and UpdateRoles on every
// engine (Postgres TEXT[], MySQL/MSSQL JSON).
func TestUserStore_RolesRoundTrip_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		store := db.NewUserStore(pool)
		email := fmt.Sprintf("roles-%s@example.com", uuid.NewString()[:8])

		u, err := store.Create(ctx, email, "!hash!", []string{"admin", "editor"}, "")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		t.Cleanup(func() { _ = store.Delete(ctx, u.ID) })
		if len(u.Roles) != 2 {
			t.Fatalf("created roles = %v, want 2", u.Roles)
		}

		u2, err := store.UpdateRoles(ctx, u.ID, []string{"viewer"})
		if err != nil {
			t.Fatalf("UpdateRoles: %v", err)
		}
		if len(u2.Roles) != 1 || u2.Roles[0] != "viewer" {
			t.Fatalf("updated roles = %v, want [viewer]", u2.Roles)
		}
	})
}

// SetRelations replaces a row's many_to_many pivot rows on every dialect: a
// second call clears the first call's rows before writing its own.
func TestContentStore_SetRelations_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg := newTestRegistry(t, pool)

		suffix := uuid.NewString()[:8]
		tagsName := "tags_" + suffix
		artName := "arts_" + suffix

		tags := &domain.Schema{Name: tagsName, Fields: []domain.SchemaField{{Name: "label", FieldType: "text"}}}
		rel := domain.SchemaField{Name: "tags", FieldType: "relation", RelationType: domain.RelManyToMany, RelationTo: tagsName}
		arts := &domain.Schema{Name: artName, Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}, rel}}

		for _, sc := range []*domain.Schema{tags, arts} {
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("engine.Apply %s: %v", sc.Name, err)
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("reg.Upsert %s: %v", sc.Name, err)
			}
			scCopy := sc
			t.Cleanup(func() { _ = reg.Delete(ctx, scCopy.Name) })
		}

		store := db.NewContentStore(pool, reg.Source())
		t1, err := store.Insert(ctx, tagsName, map[string]any{"label": "go"})
		if err != nil {
			t.Fatalf("insert tag1: %v", err)
		}
		t2, err := store.Insert(ctx, tagsName, map[string]any{"label": "rust"})
		if err != nil {
			t.Fatalf("insert tag2: %v", err)
		}
		art, err := store.Insert(ctx, artName, map[string]any{"title": "post"})
		if err != nil {
			t.Fatalf("insert article: %v", err)
		}

		if err := store.SetRelations(ctx, artName, art.ID, rel, []uuid.UUID{t1.ID, t2.ID}); err != nil {
			t.Fatalf("SetRelations (2): %v", err)
		}
		items, _, err := store.ListRelated(ctx, artName, art.ID, rel, 50, 0)
		if err != nil {
			t.Fatalf("ListRelated after set-2: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("related count = %d, want 2", len(items))
		}

		// Replace semantics: a second call must clear the prior rows first.
		if err := store.SetRelations(ctx, artName, art.ID, rel, []uuid.UUID{t1.ID}); err != nil {
			t.Fatalf("SetRelations (1): %v", err)
		}
		items, _, err = store.ListRelated(ctx, artName, art.ID, rel, 50, 0)
		if err != nil {
			t.Fatalf("ListRelated after set-1: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("related count after replace = %d, want 1", len(items))
		}
	})
}
