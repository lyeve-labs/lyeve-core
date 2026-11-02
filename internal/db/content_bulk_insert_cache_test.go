package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// A bulk create is a content write like any other, so a list cached before it
// must not outlive it, here or on the other replicas.
func TestContentStore_BulkInsertFlushesCachedLists_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		schemas := newTestRegistry(t, pool)
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name:   "bulk_flush",
			Fields: []domain.SchemaField{{Name: "title", FieldType: "text"}},
		}))
		store := db.NewContentStoreWithCache(pool, schemas.Source(),
			cache.NewMemory[string, *domain.Content](100, 0), cache.NewMemory[string, []*domain.Content](100, 0))
		var told []string
		store.OnInvalidate(func(_ context.Context, schemaName string) { told = append(told, schemaName) })

		_, err := store.Insert(ctx, "bulk_flush", map[string]any{"title": "existing"})
		require.NoError(t, err)
		told = nil
		list, err := store.List(ctx, "bulk_flush", 10, 0, nil)
		require.NoError(t, err)
		require.Len(t, list, 1)

		_, err = store.BulkInsert(ctx, "bulk_flush", []map[string]any{{"title": "first"}, {"title": "second"}})
		require.NoError(t, err)

		list, err = store.List(ctx, "bulk_flush", 10, 0, nil)
		require.NoError(t, err)
		assert.Len(t, list, 3, "the list read after a bulk create holds the new rows")
		assert.Equal(t, []string{"bulk_flush"}, told, "the other replicas are told")
	})
}
