//go:build !mutest

package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// relationStoreFixture is two tenants on tenant-scoped connections, each with
// rows in the shared tables a relation can point into.
type relationStoreFixture struct {
	pool       db.DB
	store      *db.ContentStore
	ctxA, ctxB context.Context

	ourAuthor, theirAuthor uuid.UUID
	ourTopic, theirTopic   uuid.UUID
	ourPost, theirPost     uuid.UUID
}

var relTopicsField = domain.SchemaField{Name: "topics", FieldType: "relation", RelationType: domain.RelManyToMany, RelationTo: "rta_topics"}

func newRelationStoreFixture(t *testing.T, pool db.DB) relationStoreFixture {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	slugA, slugB := "rel_a_"+suffix, "rel_b_"+suffix
	createTenant(t, ctx, pool, slugA)
	createTenant(t, ctx, pool, slugB)
	t.Cleanup(func() {
		dropTenant(t, pool, slugA)
		dropTenant(t, pool, slugB)
	})
	baseA, connA := acquireTenant(t, ctx, pool, slugA)
	t.Cleanup(func() { _ = connA.Close() })
	baseB, connB := acquireTenant(t, ctx, pool, slugB)
	t.Cleanup(func() { _ = connB.Close() })

	reg := newTestRegistry(t, pool)
	for _, sc := range []*domain.Schema{
		{Name: "rta_authors", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}},
		{Name: "rta_topics", Fields: []domain.SchemaField{{Name: "name", FieldType: "text"}}},
		{Name: "rta_posts", Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text"},
			{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "rta_authors"},
			relTopicsField,
		}},
	} {
		if err := reg.Upsert(ctx, sc); err != nil {
			t.Fatalf("apply %s: %v", sc.Name, err)
		}
	}

	fx := relationStoreFixture{
		pool:  pool,
		store: db.NewContentStore(pool, reg.Source()),
		ctxA:  tenant.WithID(baseA, slugA),
		ctxB:  tenant.WithID(baseB, slugB),
	}
	fx.ourAuthor = fx.insert(t, fx.ctxA, "rta_authors", map[string]any{"name": "ours"})
	fx.theirAuthor = fx.insert(t, fx.ctxB, "rta_authors", map[string]any{"name": "theirs"})
	fx.ourTopic = fx.insert(t, fx.ctxA, "rta_topics", map[string]any{"name": "ours"})
	fx.theirTopic = fx.insert(t, fx.ctxB, "rta_topics", map[string]any{"name": "theirs"})
	fx.ourPost = fx.insert(t, fx.ctxA, "rta_posts", map[string]any{"title": "ours"})
	fx.theirPost = fx.insert(t, fx.ctxB, "rta_posts", map[string]any{"title": "theirs"})
	return fx
}

func (fx relationStoreFixture) insert(t *testing.T, ctx context.Context, schema string, data map[string]any) uuid.UUID {
	t.Helper()
	c, err := fx.store.Insert(ctx, schema, data)
	if err != nil {
		t.Fatalf("insert %s: %v", schema, err)
	}
	return c.ID
}

// refusedField returns the field a relation refusal names, failing the test
// for any other error.
func refusedField(t *testing.T, err error) string {
	t.Helper()
	var refused *core.RelationTargetError
	if !errors.As(err, &refused) {
		t.Fatalf("want a relation refusal, got %v", err)
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a relation refusal must read as a validation error: %v", err)
	}
	return refused.Field
}

// postState is what tenant A's posts hold: every title, the stored author of
// the post the writers update, and the pivot rows naming it in any tenant.
func (fx relationStoreFixture) postState(t *testing.T) string {
	t.Helper()
	items, err := fx.store.List(fx.ctxA, "rta_posts", 100, 0, nil)
	if err != nil {
		t.Fatalf("list posts: %v", err)
	}
	var titles []string
	for _, it := range items {
		titles = append(titles, fmt.Sprint(it.Data["title"]))
	}
	own, err := fx.store.GetByID(fx.ctxA, "rta_posts", fx.ourPost)
	if err != nil {
		t.Fatalf("get post: %v", err)
	}
	row, err := fx.pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM "+core.PivotTableName("rta_posts", "rta_topics")+" WHERE rta_posts_id = $1", fx.ourPost)
	if err != nil {
		t.Fatalf("count pivot: %v", err)
	}
	var pivots int
	if err := row.Scan(&pivots); err != nil {
		t.Fatalf("count pivot: %v", err)
	}
	return fmt.Sprintf("titles=%d author=%v pivots=%d", len(titles), own.Data["author_id"], pivots)
}

type relationWriter struct {
	name   string
	theirs func(fx relationStoreFixture) uuid.UUID
	ours   func(fx relationStoreFixture) uuid.UUID
	field  string
	write  func(fx relationStoreFixture, target uuid.UUID) error
}

var relationWriters = []relationWriter{
	{
		name: "insert", field: "author",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirAuthor },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourAuthor },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			_, err := fx.store.Insert(fx.ctxA, "rta_posts", map[string]any{"title": "new", "author": target.String()})
			return err
		},
	},
	{
		name: "insert under an id", field: "author_id",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirAuthor },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourAuthor },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			_, err := fx.store.InsertWithID(fx.ctxA, "rta_posts", uuid.New(), map[string]any{"title": "new", "author_id": target.String()})
			return err
		},
	},
	{
		name: "upsert of a new row", field: "author",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirAuthor },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourAuthor },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			return fx.store.UpsertContent(fx.ctxA, "rta_posts", uuid.New(), map[string]any{"title": "new", "author": target.String()})
		},
	},
	{
		name: "upsert of a stored row", field: "author",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirAuthor },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourAuthor },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			return fx.store.UpsertContent(fx.ctxA, "rta_posts", fx.ourPost, map[string]any{"title": "ours", "author": target.String()})
		},
	},
	{
		name: "update", field: "author_id",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirAuthor },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourAuthor },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			_, err := fx.store.Update(fx.ctxA, "rta_posts", fx.ourPost, map[string]any{"author_id": target}, time.Time{})
			return err
		},
	},
	{
		name: "bulk insert", field: "author",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirAuthor },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourAuthor },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			_, err := fx.store.BulkInsert(fx.ctxA, "rta_posts", []map[string]any{
				{"title": "first"},
				{"title": "second", "author": target.String()},
			})
			return err
		},
	},
	{
		name: "many_to_many target", field: "topics",
		theirs: func(fx relationStoreFixture) uuid.UUID { return fx.theirTopic },
		ours:   func(fx relationStoreFixture) uuid.UUID { return fx.ourTopic },
		write: func(fx relationStoreFixture, target uuid.UUID) error {
			return fx.store.SetRelations(fx.ctxA, "rta_posts", fx.ourPost, relTopicsField, []uuid.UUID{target})
		},
	},
}

// Every write path of the store refuses a relation to another tenant's row,
// writes nothing, and refuses an id nobody holds in exactly the same words.
func TestContentStore_RelationToAnotherTenantsRowIsRefused(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		fx := newRelationStoreFixture(t, pool)
		before := fx.postState(t)
		for _, w := range relationWriters {
			t.Run(w.name, func(t *testing.T) {
				theirs := w.write(fx, w.theirs(fx))
				if got := refusedField(t, theirs); got != w.field {
					t.Fatalf("refusal names %q, want %q", got, w.field)
				}
				missing := w.write(fx, uuid.New())
				refusedField(t, missing)
				if theirs.Error() != missing.Error() {
					t.Fatalf("another tenant's row reads %q, a missing row %q", theirs, missing)
				}
				if after := fx.postState(t); after != before {
					t.Fatalf("a refused write changed the posts: %s, was %s", after, before)
				}
			})
		}

		t.Run("many_to_many source", func(t *testing.T) {
			theirs := fx.store.SetRelations(fx.ctxA, "rta_posts", fx.theirPost, relTopicsField, []uuid.UUID{fx.ourTopic})
			missing := fx.store.SetRelations(fx.ctxA, "rta_posts", uuid.New(), relTopicsField, []uuid.UUID{fx.ourTopic})
			if !errors.Is(theirs, domain.ErrNotFound) || !errors.Is(missing, domain.ErrNotFound) {
				t.Fatalf("want not found for both, got %v and %v", theirs, missing)
			}
			row, err := fx.pool.QueryRow(context.Background(),
				"SELECT COUNT(*) FROM "+core.PivotTableName("rta_posts", "rta_topics")+" WHERE rta_posts_id = $1", fx.theirPost)
			if err != nil {
				t.Fatalf("count pivot: %v", err)
			}
			var n int
			if err := row.Scan(&n); err != nil || n != 0 {
				t.Fatalf("pivot rows naming another tenant's post: %d, %v", n, err)
			}
		})
	})
}

func TestContentStore_RelationToOwnRowIsWritten(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		fx := newRelationStoreFixture(t, pool)
		for _, w := range relationWriters {
			t.Run(w.name, func(t *testing.T) {
				if err := w.write(fx, w.ours(fx)); err != nil {
					t.Fatalf("write: %v", err)
				}
			})
		}
		got, err := fx.store.GetByID(fx.ctxA, "rta_posts", fx.ourPost)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// MSSQL hands a UNIQUEIDENTIFIER back upper-cased.
		if author, _ := got.Data["author_id"].(string); !strings.EqualFold(author, fx.ourAuthor.String()) {
			t.Fatalf("author_id = %v, want %s", got.Data["author_id"], fx.ourAuthor)
		}
		related, total, err := fx.store.ListRelated(fx.ctxA, "rta_posts", fx.ourPost, relTopicsField, 10, 0, true)
		if err != nil || total != 1 || len(related) != 1 || related[0].ID != fx.ourTopic {
			t.Fatalf("related topics = %v (%d), %v", related, total, err)
		}
	})
}

// A declared default is written when the data leaves the relation out, so it
// is held to the tenant the same way.
func TestContentStore_RelationDefaultIsHeldToTheTenant(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		fx := newRelationStoreFixture(t, pool)
		reg := newTestRegistry(t, pool)
		for name, def := range map[string]uuid.UUID{"rta_theirs_by_default": fx.theirAuthor, "rta_ours_by_default": fx.ourAuthor} {
			if err := reg.Upsert(context.Background(), &domain.Schema{Name: name, Fields: []domain.SchemaField{
				{Name: "title", FieldType: "text"},
				{Name: "author", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "rta_authors", Default: def.String()},
			}}); err != nil {
				t.Fatalf("apply %s: %v", name, err)
			}
		}
		_, err := fx.store.Insert(fx.ctxA, "rta_theirs_by_default", map[string]any{"title": "x"})
		if got := refusedField(t, err); got != "author" {
			t.Fatalf("refusal names %q", got)
		}
		if _, err := fx.store.Insert(fx.ctxA, "rta_ours_by_default", map[string]any{"title": "x"}); err != nil {
			t.Fatalf("insert with our default: %v", err)
		}
	})
}
