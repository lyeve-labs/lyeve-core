//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// deadlockOnce wraps a real pool and reports a SQL Server deadlock on the first
// write, then steps aside. Fault injection around a live database, so the SQL
// itself is still executed and verified by the engine.
type deadlockOnce struct {
	db.DB
	fired bool
}

func (d *deadlockOnce) deadlock() error {
	d.fired = true
	return mssql.Error{Number: 1205, Message: "transaction was deadlocked and has been chosen as the deadlock victim"}
}

func (d *deadlockOnce) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if !d.fired {
		return nil, d.deadlock()
	}
	return d.DB.Exec(ctx, q, args...)
}

func (d *deadlockOnce) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	if !d.fired {
		return nil, d.deadlock()
	}
	return d.DB.QueryRow(ctx, q, args...)
}

// A deadlock victim's statement never landed, so the write is rerun rather than
// reported to the caller as an outage.
func TestContentStore_UpdateRetriesADeadlockVictim(t *testing.T) {
	pool := testdb.Postgres(t)
	ctx := context.Background()

	schemas := newTestRegistry(t, pool)
	sc := &domain.Schema{
		Name:          "deadlock_probe",
		WithCreatedAt: true,
		WithUpdatedAt: true,
		Fields:        []domain.SchemaField{{Name: "title", FieldType: "text"}},
	}
	// Applying the definition creates the table.
	require.NoError(t, schemas.Upsert(ctx, sc))

	store := db.NewContentStore(pool, schemas.Source())
	created, err := store.Insert(ctx, "deadlock_probe", map[string]any{"title": "before"})
	require.NoError(t, err)

	flaky := &deadlockOnce{DB: pool}
	retrying := db.NewContentStore(flaky, schemas.Source())

	got, err := retrying.Update(ctx, "deadlock_probe", created.ID,
		map[string]any{"title": "after"}, time.Time{})
	require.NoError(t, err, "a deadlock victim must be retried, not returned")
	require.NotNil(t, got)
	assert.True(t, flaky.fired, "the injected deadlock never fired")

	row, err := pool.QueryRow(ctx, `SELECT title FROM "_deadlock_probe" WHERE id = $1`, created.ID)
	require.NoError(t, err)
	var title string
	require.NoError(t, row.Scan(&title))
	assert.Equal(t, "after", title)
}

// Anything that is not a concurrency loss still reaches the caller unchanged.
func TestContentStore_UpdateDoesNotRetryAConflict(t *testing.T) {
	pool := testdb.Postgres(t)
	ctx := context.Background()

	schemas := newTestRegistry(t, pool)
	require.NoError(t, schemas.Upsert(ctx, &domain.Schema{
		Name:          "conflict_probe",
		WithCreatedAt: true,
		WithUpdatedAt: true,
		Fields:        []domain.SchemaField{{Name: "title", FieldType: "text"}},
	}))

	store := db.NewContentStore(pool, schemas.Source())
	created, err := store.Insert(ctx, "conflict_probe", map[string]any{"title": "before"})
	require.NoError(t, err)

	_, err = store.Update(ctx, "conflict_probe", created.ID,
		map[string]any{"title": "after"}, time.Now().Add(-time.Hour))
	assert.ErrorIs(t, err, db.ErrContentConflict)

	_, err = store.Update(ctx, "conflict_probe", uuid.New(),
		map[string]any{"title": "after"}, time.Time{})
	assert.ErrorIs(t, err, db.ErrContentConflict)
}
