package db_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A belongs_to into rows that pass the walk's byte bound together stops
// reading at the row that passes it. Every row the store reads lands in its
// item cache, so the cache counts the rows the refused read held in memory.
func TestContentStore_PopulateBelongsToStopsReadingAtTheByteBound_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		schemas := newTestRegistry(t, pool)
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name:   "bound_blobs",
			Fields: []domain.SchemaField{{Name: "body", FieldType: "text"}},
		}))
		t.Cleanup(func() { _ = schemas.Delete(context.Background(), "bound_blobs") })
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name: "bound_refs",
			Fields: []domain.SchemaField{
				{Name: "blob", FieldType: "relation", RelationTo: "bound_blobs", RelationType: domain.RelBelongsTo},
			},
		}))
		t.Cleanup(func() { _ = schemas.Delete(context.Background(), "bound_refs") })

		seeder := db.NewContentStore(pool, schemas.Source())
		body := strings.Repeat("x", 1<<20)
		const blobs = 48
		items := make([]*domain.Content, 0, blobs)
		for range blobs {
			c, err := seeder.Insert(ctx, "bound_blobs", map[string]any{"body": body})
			require.NoError(t, err)
			items = append(items, &domain.Content{
				SchemaName: "bound_refs",
				Data:       map[string]any{domain.FKColumn("blob", ""): c.ID.String()},
			})
		}

		itemCache := &countingItemCache{Cache: cache.NewMemory[string, *domain.Content](1000, 0)}
		store := db.NewContentStoreWithCache(pool, schemas.Source(), itemCache, cache.NewMemory[string, []*domain.Content](100, 0))
		err := store.PopulateStrict(ctx, items, domain.PopulateConfig{Paths: []string{"blob"}}, nil)
		require.ErrorIs(t, err, db.ErrPopulateTooLarge)

		read := int(itemCache.sets.Load())
		assert.LessOrEqual(t, read, 34, "the refused read held %d of %d rows of 1 MiB", read, blobs)
	})
}

// A belongs_to target the caller's mask hides is not walked below, so what
// lies under a draft the caller may not read never counts toward the bounds.
// The draft author here carries more bytes below it than a walk may read: a
// reader who may not read drafts gets the post with no author, and one who
// may still meets the bound.
func TestContentStore_PopulateSkipsBelowAHiddenEntry_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		schemas := newTestRegistry(t, pool)
		authors := &domain.Schema{
			Name:             "hidden_authors",
			WithDraftPublish: true,
			Fields:           []domain.SchemaField{{Name: "name", FieldType: "text"}},
		}
		require.NoError(t, schemas.Upsert(ctx, authors))
		t.Cleanup(func() { _ = schemas.Delete(context.Background(), "hidden_authors") })
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name: "hidden_notes",
			Fields: []domain.SchemaField{
				{Name: "body", FieldType: "text"},
				{Name: "author", FieldType: "relation", RelationTo: "hidden_authors", RelationType: domain.RelBelongsTo},
			},
		}))
		t.Cleanup(func() { _ = schemas.Delete(context.Background(), "hidden_notes") })
		authors.Fields = append(authors.Fields, domain.SchemaField{Name: "notes", FieldType: "relation", RelationTo: "hidden_notes", RelationType: domain.RelHasMany})
		require.NoError(t, schemas.Upsert(ctx, authors))
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name: "hidden_posts",
			Fields: []domain.SchemaField{
				{Name: "author", FieldType: "relation", RelationTo: "hidden_authors", RelationType: domain.RelBelongsTo},
			},
		}))
		t.Cleanup(func() { _ = schemas.Delete(context.Background(), "hidden_posts") })

		store := db.NewContentStore(pool, schemas.Source())
		author, err := store.Insert(ctx, "hidden_authors", map[string]any{"name": "a", "_status": "draft"})
		require.NoError(t, err)
		body := strings.Repeat("x", 1<<20)
		for range 34 {
			_, err := store.Insert(ctx, "hidden_notes", map[string]any{"body": body, "author": author.ID.String()})
			require.NoError(t, err)
		}
		post := func() *domain.Content {
			return &domain.Content{
				SchemaName: "hidden_posts",
				Data:       map[string]any{domain.FKColumn("author", ""): author.ID.String()},
			}
		}
		cfg := domain.PopulateConfig{Paths: []string{"author.notes"}}

		published := func(context.Context, domain.SchemaField) (func(map[string]any) map[string]any, func(string) bool, bool, bool) {
			mask := func(data map[string]any) map[string]any {
				if !core.ContentPublished(data) {
					return nil
				}
				return data
			}
			return mask, nil, false, true
		}
		reader := post()
		require.NoError(t, store.PopulateStrict(ctx, []*domain.Content{reader}, cfg, published))
		assert.Nil(t, reader.Data["author"])

		editor := post()
		require.ErrorIs(t, store.PopulateStrict(ctx, []*domain.Content{editor}, cfg, nil), db.ErrPopulateTooLarge)
	})
}

// countingItemCache counts the rows written into an item cache, which the
// store does once for each row it reads from the database.
type countingItemCache struct {
	cache.Cache[string, *domain.Content]
	sets atomic.Int64
}

func (c *countingItemCache) Set(ctx context.Context, key string, value *domain.Content, ttl time.Duration) error {
	c.sets.Add(1)
	return c.Cache.Set(ctx, key, value, ttl)
}
