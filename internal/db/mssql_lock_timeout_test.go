package db_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The driver resets a session each time the pool hands its connection out
// again, and the reset puts LOCK_TIMEOUT back to the server's default of -1,
// which waits forever. The pool's limit has to be set again after every
// reset, not only when the connection opens.
func TestMSSQLLockTimeout_SurvivesSessionReset(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}
	ctx := context.Background()
	_, dsn := testdb.MSSQLWithDSN(t)
	pool, err := db.ConnectWithOptions(ctx, dsn, db.ConnectOptions{MaxConns: 1, StatementTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { _ = pool.Close() }()

	// One open connection, so every query after the ping reuses the session
	// the ping opened.
	var firstSPID int
	for i := range 3 {
		var spid, lockTimeout int
		require.NoError(t, pool.SQLDB().QueryRowContext(ctx, `SELECT @@SPID, @@LOCK_TIMEOUT`).Scan(&spid, &lockTimeout))
		if i == 0 {
			firstSPID = spid
		}
		require.Equal(t, firstSPID, spid, "query %d ran on another session, so it proves nothing about a reset", i)
		assert.Equal(t, 2000, lockTimeout, "@@LOCK_TIMEOUT on query %d", i)
	}
}

// A SET LOCK_TIMEOUT a caller runs inside one borrow ends with that borrow.
// The next borrower of the same session reads the pool's limit, not the
// caller's.
func TestMSSQLLockTimeout_CallerSetDoesNotLeakToNextBorrow(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}
	ctx := context.Background()
	_, dsn := testdb.MSSQLWithDSN(t)
	pool, err := db.ConnectWithOptions(ctx, dsn, db.ConnectOptions{MaxConns: 1, StatementTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { _ = pool.Close() }()
	sqlDB := pool.SQLDB()

	first, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	_, err = first.ExecContext(ctx, `SET LOCK_TIMEOUT 5`)
	require.NoError(t, err)
	var firstSPID, inside int
	require.NoError(t, first.QueryRowContext(ctx, `SELECT @@SPID, @@LOCK_TIMEOUT`).Scan(&firstSPID, &inside))
	require.Equal(t, 5, inside, "the caller's SET did not take inside its own borrow")
	require.NoError(t, first.Close())

	next, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = next.Close() }()
	var spid, lockTimeout int
	require.NoError(t, next.QueryRowContext(ctx, `SELECT @@SPID, @@LOCK_TIMEOUT`).Scan(&spid, &lockTimeout))
	require.Equal(t, firstSPID, spid, "the second borrow got another session, so it proves nothing about a leak")
	assert.Equal(t, 2000, lockTimeout)
}

// A statement blocked by another session's lock fails once the pool's limit
// has passed, instead of holding its connection until the caller's context
// ends.
func TestMSSQLLockTimeout_BlockedStatementFailsAtTheLimit(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}
	ctx := context.Background()
	_, dsn := testdb.MSSQLWithDSN(t)
	pool, err := db.ConnectWithOptions(ctx, dsn, db.ConnectOptions{MaxConns: 2, StatementTimeout: time.Second})
	require.NoError(t, err)
	defer func() { _ = pool.Close() }()
	sqlDB := pool.SQLDB()

	_, err = sqlDB.ExecContext(ctx, `CREATE TABLE lock_timeout_probe (id INT PRIMARY KEY, n INT NOT NULL)`)
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO lock_timeout_probe (id, n) VALUES (1, 0)`)
	require.NoError(t, err)

	// Open both connections and hand them back, so the two used below are
	// reused sessions the driver resets.
	holder, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	waiter, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	require.NoError(t, holder.Close())
	require.NoError(t, waiter.Close())

	holder, err = sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = holder.Close() }()
	tx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE lock_timeout_probe SET n = 1 WHERE id = 1`)
	require.NoError(t, err)

	waiter, err = sqlDB.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = waiter.Close() }()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err = waiter.ExecContext(waitCtx, `UPDATE lock_timeout_probe SET n = 2 WHERE id = 1`)
	elapsed := time.Since(start)

	require.Error(t, err, "the blocked update succeeded, so nothing held the lock")
	assert.NotErrorIs(t, err, context.DeadlineExceeded, "the update waited for the context, not the lock limit")
	assert.NotErrorIs(t, err, sql.ErrConnDone)
	assert.Contains(t, err.Error(), "Lock request time out period exceeded")
	assert.Less(t, elapsed, 10*time.Second, "the update waited %s for a one second lock limit", elapsed)
}
