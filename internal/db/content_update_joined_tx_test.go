//go:build !mutest

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/cache"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// The caller below loses a real deadlock while it holds a transaction the
// store joined. MySQL and SQL Server roll that transaction back whole, so a
// statement the store ran again afterwards would commit on its own, outside
// it, and PostgreSQL refuses every statement after the deadlock. Only the
// caller can start the transaction over, so the store hands the deadlock to
// it and runs nothing more.
func TestContentStore_UpdateHandsADeadlockToTheTransactionOwner_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		schemas := newTestRegistry(t, pool)
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name:          "deadlock_join",
			WithCreatedAt: true,
			WithUpdatedAt: true,
			Fields:        []domain.SchemaField{{Name: "title", FieldType: "text"}},
		}))
		for _, ddl := range []string{
			`CREATE TABLE deadlock_marker (id VARCHAR(64) NOT NULL PRIMARY KEY)`,
			`CREATE TABLE deadlock_filler (id VARCHAR(64) NOT NULL PRIMARY KEY)`,
		} {
			_, err := pool.Exec(ctx, ddl)
			require.NoError(t, err)
		}
		store := db.NewContentStore(pool, schemas.Source())
		a, err := store.Insert(ctx, "deadlock_join", map[string]any{"title": "a"})
		require.NoError(t, err)
		b, err := store.Insert(ctx, "deadlock_join", map[string]any{"title": "b"})
		require.NoError(t, err)
		table := domain.TableName("deadlock_join")

		callerHoldsA, otherHoldsB := make(chan struct{}), make(chan struct{})
		otherDone := make(chan error, 1)
		go func() {
			otherDone <- db.WriteInTransaction(ctx, pool, func(ctx context.Context) error {
				// The other writer has to survive the deadlock on every
				// dialect: SQL Server spares the higher priority, MySQL the
				// transaction that changed more rows, and PostgreSQL the
				// one that started waiting last.
				if pool.Engine() == "mssql" {
					if _, err := pool.Exec(ctx, `SET DEADLOCK_PRIORITY HIGH`); err != nil {
						return err
					}
					defer pool.Exec(ctx, `SET DEADLOCK_PRIORITY NORMAL`) //nolint:errcheck // the session goes back to the pool
				}
				for i := range 50 {
					if _, err := pool.Exec(ctx, `INSERT INTO deadlock_filler (id) VALUES ($1)`, fmt.Sprintf("f%02d", i)); err != nil {
						return err
					}
				}
				<-callerHoldsA
				if _, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET title = 'other' WHERE id = $1`, table), b.ID); err != nil {
					return err
				}
				close(otherHoldsB)
				time.Sleep(300 * time.Millisecond)
				_, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET title = 'other' WHERE id = $1`, table), a.ID)
				return err
			})
		}()

		err = db.SerializeWrite(ctx, pool, "test.deadlock_join", func(ctx context.Context) error {
			if _, err := pool.Exec(ctx, `INSERT INTO deadlock_marker (id) VALUES ($1)`, uuid.NewString()); err != nil {
				return err
			}
			if _, err := store.Update(ctx, "deadlock_join", a.ID, map[string]any{"title": "caller"}, time.Time{}); err != nil {
				close(callerHoldsA)
				return err
			}
			close(callerHoldsA)
			<-otherHoldsB
			_, err := store.Update(ctx, "deadlock_join", b.ID, map[string]any{"title": "caller"}, time.Time{})
			return err
		})
		require.NoError(t, <-otherDone, "the other writer must win the deadlock")

		assert.Error(t, err, "the caller lost its transaction and must learn it")
		assert.True(t, sqlx.IsRetryableTxError(err), "the caller must be able to retry its transaction: %v", err)

		row, qerr := pool.QueryRow(ctx, `SELECT COUNT(*) FROM deadlock_marker`)
		require.NoError(t, qerr)
		var markers int
		require.NoError(t, row.Scan(&markers))
		assert.Zero(t, markers, "the caller's transaction rolled back")
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			row, qerr := pool.QueryRow(ctx, fmt.Sprintf(`SELECT title FROM %s WHERE id = $1`, table), id)
			require.NoError(t, qerr)
			var title string
			require.NoError(t, row.Scan(&title))
			assert.Equal(t, "other", title, "no write of the caller may land outside its transaction")
		}
	})
}

// A reader that misses the cache while a caller's transaction holds an update
// reads the row as committed, the old one, and caches it. The store drops the
// cached row when the caller's transaction commits rather than when its own
// statement completes, so the next read sees the update. A read inside the
// transaction goes to the database, so the caller sees its own write and the
// cache never holds a row that has not committed.
func TestContentStore_UpdateInACallersTransactionDropsTheCacheAfterCommit_AllDialects(t *testing.T) {
	eachDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		schemas := newTestRegistry(t, pool)
		require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
			Name:          "cache_join",
			WithCreatedAt: true,
			WithUpdatedAt: true,
			Fields:        []domain.SchemaField{{Name: "title", FieldType: "text"}},
		}))
		store := db.NewContentStoreWithCache(pool, schemas.Source(),
			cache.NewMemory[string, *domain.Content](100, 0), cache.NewMemory[string, []*domain.Content](100, 0))
		row, err := store.Insert(ctx, "cache_join", map[string]any{"title": "before"})
		require.NoError(t, err)

		read := make(chan error, 1)
		err = db.SerializeWrite(ctx, pool, "test.cache_join", func(ctx context.Context) error {
			if _, err := store.Update(ctx, "cache_join", row.ID, map[string]any{"title": "after"}, time.Time{}); err != nil {
				return err
			}
			go func() {
				_, err := store.GetByID(context.Background(), "cache_join", row.ID)
				read <- err
			}()
			// SQL Server's locking read waits for the commit, so the wait
			// is bounded and the commit goes ahead.
			select {
			case err := <-read:
				read <- err
			case <-time.After(500 * time.Millisecond):
			}
			inside, err := store.GetByID(ctx, "cache_join", row.ID)
			if err != nil {
				return err
			}
			assert.Equal(t, "after", inside.Data["title"], "the caller must read its own write")
			return nil
		})
		require.NoError(t, err)
		require.NoError(t, <-read)

		got, err := store.GetByID(ctx, "cache_join", row.ID)
		require.NoError(t, err)
		assert.Equal(t, "after", got.Data["title"], "a read after the commit must not be served the row cached before it")
	})
}
