package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// A has_one, has_many or many_to_many keeps nothing on this table, and a
// client has no way to know that from the field list. A value sent under one
// of their names would reach the INSERT as a column that does not exist, so
// the write takes what it stores and ignores the rest.
func TestContentStore_IgnoresAValueForAnInverseRelation(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg := newTestRegistry(t, pool)
		apply := func(sc *domain.Schema) {
			t.Helper()
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("apply %s: %v", sc.Name, err)
			}
			if err := reg.Upsert(ctx, sc); err != nil {
				t.Fatalf("upsert %s: %v", sc.Name, err)
			}
		}
		apply(&domain.Schema{Name: "irw_authors", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}})
		apply(&domain.Schema{Name: "irw_tags", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}})
		apply(&domain.Schema{
			Name: "irw_posts",
			Fields: []domain.SchemaField{
				{Name: "title", FieldType: "text"},
				{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "irw_authors"},
				{Name: "editor", FieldType: "relation", RelationType: domain.RelHasOne, RelationTo: "irw_authors"},
				{Name: "readers", FieldType: "relation", RelationType: domain.RelHasMany, RelationTo: "irw_authors"},
				{Name: "tags", FieldType: "relation", RelationType: domain.RelManyToMany, RelationTo: "irw_tags"},
			},
		})

		store := db.NewContentStore(pool, reg.Source())
		author, err := store.Insert(ctx, "irw_authors", map[string]any{"name": "a"})
		if err != nil {
			t.Fatalf("insert author: %v", err)
		}

		post, err := store.Insert(ctx, "irw_posts", map[string]any{
			"title":   "t",
			"author":  author.ID.String(),
			"editor":  author.ID.String(),
			"readers": []any{author.ID.String()},
			"tags":    []any{},
		})
		if err != nil {
			t.Fatalf("insert with inverse values: %v", err)
		}
		// The belongs_to beside them still has to land, read back from the
		// table rather than from the echo of the input.
		stored, err := store.GetByID(ctx, "irw_posts", post.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// MSSQL hands a UNIQUEIDENTIFIER back upper-cased.
		if got, _ := stored.Data["author_id"].(string); !strings.EqualFold(got, author.ID.String()) {
			t.Fatalf("author_id = %q, data = %v", got, stored.Data)
		}

		if _, err := store.Update(ctx, "irw_posts", post.ID, map[string]any{
			"title":   "t2",
			"editor":  author.ID.String(),
			"readers": []any{author.ID.String()},
		}, post.UpdatedAt); err != nil {
			t.Fatalf("update with inverse values: %v", err)
		}
	})
}
