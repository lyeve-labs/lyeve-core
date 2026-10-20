package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// noTenant scopes a connection to the engine's own schema.
func noTenant(context.Context) string { return "" }

// readOne runs one read with ctx and reports how many pool connections were
// in use while its row was still open.
func readOne(t *testing.T, pool db.DB, ctx context.Context) int {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT COUNT(*) FROM sys_users`)
	require.NoError(t, err)
	inUse := pool.Stats().InUse
	require.NoError(t, rows.Close())
	return inUse
}

// A read made before the request has taken its tenant connection leaves it
// untaken and gives its own connection straight back.
func TestWithoutTakingTenantConn_LeavesTheRequestConnUntaken(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	lc := db.NewLazyTenantConn(pool.SQLDB(), db.NewPostgresSchemaTenancy(noTenant))
	ctx := db.WithLazyTenantConn(context.Background(), lc)

	assert.Equal(t, 1, readOne(t, pool, db.WithoutTakingTenantConn(ctx)))
	assert.Nil(t, lc.Acquired(), "the read took the request's connection")
	assert.Equal(t, 0, pool.Stats().InUse)
}

// Once the request holds its connection, the read runs on it rather than
// asking the pool for a second one while it holds the first.
func TestWithoutTakingTenantConn_ReadsOnAConnTheRequestHolds(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	lc := db.NewLazyTenantConn(pool.SQLDB(), db.NewPostgresSchemaTenancy(noTenant))
	ctx := db.WithLazyTenantConn(context.Background(), lc)
	held, err := lc.Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	assert.Equal(t, 1, readOne(t, pool, db.WithoutTakingTenantConn(ctx)), "the read borrowed a second connection")
}
