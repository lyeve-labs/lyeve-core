package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// A content model that points at one schema twice: posts have an author who
// must exist, an editor who need not, and a set of reviewers through a
// pivot, all of them authors. Comments hang off posts and must have one.
// SQL Server refuses a second cascading key, so the engine takes the
// referential actions on itself there, and the three dialects have to agree
// on what deleting an author does.
func applyAuthorsPostsComments(t *testing.T, pool db.DB) (*testRegistry, *db.ContentStore) {
	t.Helper()
	ctx := context.Background()
	reg := newTestRegistry(t, pool)
	for _, sc := range []*domain.Schema{
		{Name: "rda_authors", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}},
		{Name: "rda_posts", Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "rda_authors", Required: true},
			{Name: "editor", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "rda_authors"},
			{Name: "reviewers", FieldType: "relation", RelationType: domain.RelManyToMany, RelationTo: "rda_authors"},
		}},
		{Name: "rda_comments", Fields: []domain.SchemaField{
			{Name: "body", FieldType: "text"},
			{Name: "post", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "rda_posts", Required: true},
		}},
	} {
		if err := reg.Upsert(ctx, sc); err != nil {
			t.Fatalf("apply %s: %v", sc.Name, err)
		}
		if err := reg.Upsert(ctx, sc); err != nil {
			t.Fatalf("upsert %s: %v", sc.Name, err)
		}
	}
	return reg, db.NewContentStore(pool, reg.Source())
}

func TestSchema_TwoRelationsToOneSchemaApplyOnEveryDialect(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		applyAuthorsPostsComments(t, pool)
	})
}

func TestContentStore_DeleteAppliesEveryReferentialAction(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		reg, store := applyAuthorsPostsComments(t, pool)
		insert := func(schema string, data map[string]any) uuid.UUID {
			t.Helper()
			c, err := store.Insert(ctx, schema, data)
			if err != nil {
				t.Fatalf("insert %s: %v", schema, err)
			}
			return c.ID
		}
		gone := func(schema string, id uuid.UUID) bool {
			t.Helper()
			_, err := store.GetByID(ctx, schema, id)
			if err == nil {
				return false
			}
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("get %s: %v", schema, err)
			}
			return true
		}

		ann := insert("rda_authors", map[string]any{"name": "ann"})
		bob := insert("rda_authors", map[string]any{"name": "bob"})
		byAnn := insert("rda_posts", map[string]any{"title": "by ann", "author": ann.String(), "editor": bob.String()})
		byBob := insert("rda_posts", map[string]any{"title": "by bob", "author": bob.String(), "editor": ann.String()})
		onAnns := insert("rda_comments", map[string]any{"body": "c", "post": byAnn.String()})
		onBobs := insert("rda_comments", map[string]any{"body": "c", "post": byBob.String()})

		posts, err := reg.GetByName(ctx, "rda_posts")
		if err != nil {
			t.Fatal(err)
		}
		var reviewers domain.SchemaField
		for _, f := range posts.Fields {
			if f.Name == "reviewers" {
				reviewers = f
			}
		}
		if err := store.SetRelations(ctx, "rda_posts", byBob, reviewers, []uuid.UUID{ann, bob}); err != nil {
			t.Fatalf("set reviewers: %v", err)
		}

		if err := store.Delete(ctx, "rda_authors", ann); err != nil {
			t.Fatalf("delete ann: %v", err)
		}

		// Required: her post goes, and the comment that required the post.
		if !gone("rda_posts", byAnn) {
			t.Error("a post whose required author was deleted is still there")
		}
		if !gone("rda_comments", onAnns) {
			t.Error("a comment on a deleted post is still there")
		}
		// Optional: bob's post stays and loses its editor.
		bobs, err := store.GetByID(ctx, "rda_posts", byBob)
		if err != nil {
			t.Fatalf("bob's post: %v", err)
		}
		if v := bobs.Data["editor_id"]; v != nil && v != "" {
			t.Errorf("editor_id = %v, want NULL", v)
		}
		if gone("rda_comments", onBobs) {
			t.Error("a comment on a surviving post was deleted")
		}
		// Pivot: her pairing goes, his stays.
		left, _, err := store.ListRelated(ctx, "rda_posts", byBob, reviewers, 10, 0)
		if err != nil {
			t.Fatalf("list reviewers: %v", err)
		}
		if len(left) != 1 || left[0].ID != bob {
			t.Errorf("reviewers after delete = %v, want only bob", left)
		}
		// Deleting nothing is still not found.
		if err := store.Delete(ctx, "rda_authors", ann); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("second delete: %v, want not found", err)
		}
	})
}

// The behavior test above reads a delete three tables away from the key that
// carries it. This one reads the key.
//
// MySQL parses a column-level REFERENCES clause and discards it without a
// word, so the keys have to be read back from the catalog to prove they exist.
func TestSchema_RelationsCreateRealForeignKeys(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		applyAuthorsPostsComments(t, pool)

		var q string
		switch pool.Engine() {
		case "mssql":
			q = `SELECT OBJECT_NAME(fk.parent_object_id), fk.delete_referential_action_desc
			     FROM sys.foreign_keys fk
			     WHERE OBJECT_NAME(fk.parent_object_id) IN ('_rda_posts', '_rda_comments')`
		case "mysql":
			q = `SELECT TABLE_NAME, DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS
			     WHERE CONSTRAINT_SCHEMA = DATABASE() AND TABLE_NAME IN ('_rda_posts', '_rda_comments')`
		default:
			// confdeltype is a single character, not a word.
			q = `SELECT c.conrelid::regclass::text,
			            CASE c.confdeltype WHEN 'c' THEN 'CASCADE' WHEN 'n' THEN 'SET NULL'
			                               WHEN 'a' THEN 'NO ACTION' WHEN 'r' THEN 'RESTRICT'
			                               ELSE c.confdeltype::text END
			     FROM pg_constraint c
			     WHERE c.contype = 'f' AND c.conrelid::regclass::text IN ('_rda_posts', '_rda_comments')`
		}
		rows, err := pool.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("read constraints: %v", err)
		}
		defer rows.Close()
		// Two keys on posts (author, editor) and one on comments. Each
		// carries the action the field declares, except on SQL Server, where
		// every key is NO ACTION and the content store applies the action.
		rules := map[string]int{}
		keys := 0
		for rows.Next() {
			var table, rule string
			if err := rows.Scan(&table, &rule); err != nil {
				t.Fatalf("scan: %v", err)
			}
			keys++
			rules[strings.ToUpper(strings.ReplaceAll(rule, "_", " "))]++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if keys != 3 {
			t.Fatalf("belongs_to foreign keys = %d, want 3: a relation declared as a column clause leaves no key behind", keys)
		}
		want := map[string]int{"CASCADE": 2, "SET NULL": 1}
		if pool.Engine() == "mssql" {
			want = map[string]int{"NO ACTION": 3}
		}
		for action, n := range want {
			if rules[action] != n {
				t.Errorf("keys with ON DELETE %s = %d, want %d (all: %v)", action, rules[action], n, rules)
			}
		}
	})
}
