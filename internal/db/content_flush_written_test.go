package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// A row written past the store, the way a batch import writes, stays out of a
// cached list until the store is told. FlushWritten drops the cached lists and
// rows, so the next read sees the row, and it tells the other replicas.
func TestContentStore_FlushWrittenShowsARowWrittenPastTheStore_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		schemas := newTestRegistry(t, pool)
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name:   "flush_written",
			Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}},
		}))
		cached := db.NewContentStoreWithCache(pool, schemas.Source(),
			cache.NewMemory[string, *domain.Content](100, 0), cache.NewMemory[string, []*domain.Content](100, 0))
		var told []string
		cached.OnInvalidate(func(_ context.Context, schemaName string) { told = append(told, schemaName) })
		first, err := cached.Insert(ctx, "flush_written", map[string]any{"title": "first"})
		require.NoError(t, err)
		told = nil

		list, err := cached.List(ctx, "flush_written", 10, 0, nil)
		require.NoError(t, err)
		require.Len(t, list, 1)
		_, err = cached.GetByID(ctx, "flush_written", first.ID)
		require.NoError(t, err)

		past := db.NewContentStore(pool, schemas.Source())
		_, err = past.Insert(ctx, "flush_written", map[string]any{"title": "second"})
		require.NoError(t, err)
		_, err = past.Update(ctx, "flush_written", first.ID, map[string]any{"title": "renamed"}, time.Time{})
		require.NoError(t, err)
		list, err = cached.List(ctx, "flush_written", 10, 0, nil)
		require.NoError(t, err)
		require.Len(t, list, 1, "the cached list is what a reader gets before the flush")

		cached.FlushWritten(ctx, "flush_written")
		list, err = cached.List(ctx, "flush_written", 10, 0, nil)
		require.NoError(t, err)
		assert.Len(t, list, 2, "the list read after the flush holds the row written past the store")
		got, err := cached.GetByID(ctx, "flush_written", first.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed", got.Data["title"], "a cached row goes with the lists")
		assert.Equal(t, []string{"flush_written"}, told, "the other replicas are told")
	})
}
