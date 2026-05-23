package enginehost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The pool is shared with the request path, which binds a connection to a
// tenant database on MySQL. A pooled connection keeps that binding when it is
// handed back, so the next caller issuing unqualified DDL writes into the
// wrong database, or fails with "Unknown database" once that tenant is gone.
//
// Capping the pool at one connection makes the reuse deterministic: the
// connection this test dirties is the one it gets back.
func TestEngineDBConn_RebindsAConnectionAnotherTenantLeftBound(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT != mysql; the binding this guards against exists on MySQL and SQL Server")
	}
	ctx := context.Background()
	pool := testdb.MySQL(t)

	// testdb hands out a shared pool, so the cap has to come back off or every
	// later test in this run queues behind one connection.
	raw := pool.SQLDB()
	prev := raw.Stats().MaxOpenConnections
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { raw.SetMaxOpenConns(prev) })

	var engineDB string
	row, err := pool.QueryRow(ctx, "SELECT DATABASE()")
	require.NoError(t, err)
	require.NoError(t, row.Scan(&engineDB))
	require.NotEmpty(t, engineDB)

	// Named after the engine database, which testdb makes unique per test.
	// A fixed name would collide with a package running beside this one, and
	// this test's cleanup would drop the other one's fixture.
	otherDB := "other_" + engineDB
	_, err = pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+otherDB)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+otherDB) })

	// Dirty the one connection the pool has, then hand it back.
	dirty, err := raw.Conn(ctx)
	require.NoError(t, err)
	_, err = dirty.ExecContext(ctx, "USE "+otherDB)
	require.NoError(t, err)
	require.NoError(t, dirty.Close())

	h := NewHost(pool, raw, nil, nil, "test")
	provider, ok := h.(core.EngineDBConnProvider)
	require.True(t, ok, "the engine host does not provide an engine database connection")

	conn, err := provider.EngineDBConn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	var got string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&got))
	require.Equal(t, engineDB, got,
		"the connection is still bound to the database the previous caller left it on")
}

// PostgreSQL has no per-connection database binding, so the provider documents
// that it returns an ordinary connection. This pins it, so the documented
// no-op cannot become a nil or an error unnoticed.
func TestEngineDBConn_PostgresReturnsAUsableConnection(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	ctx := context.Background()
	pool := testdb.Postgres(t)

	h := NewHost(pool, pool.SQLDB(), nil, nil, "test")
	provider, ok := h.(core.EngineDBConnProvider)
	require.True(t, ok, "the engine host does not provide an engine database connection")

	conn, err := provider.EngineDBConn(ctx)
	require.NoError(t, err)
	require.NotNil(t, conn)
	defer conn.Close()

	var one int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	require.Equal(t, 1, one)
}
