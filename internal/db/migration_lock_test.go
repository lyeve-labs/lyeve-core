package db

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openLockConn mirrors Migrate's lock setup. Closing the returned func releases
// the advisory lock: sql.Conn.Close only returns the connection to its pool, so
// the session-scoped lock survives it and only the *sql.DB close drops it.
func openLockConn(t *testing.T, dsn string) (*sql.Conn, func()) {
	t.Helper()
	db, err := sql.Open(driverNameFor("postgres"), driverDSN("postgres", dsn))
	require.NoError(t, err)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	return conn, func() {
		_ = conn.Close()
		_ = db.Close()
	}
}

// Two instances racing to migrate the same database is the normal case, not an
// edge case: a Deployment starts every replica at once. Treating a refused
// try-acquire as fatal would crash-loop every pod but one on first rollout, so
// the second caller must wait and then proceed.
func TestMigrationLock_SecondCallerWaitsThenProceeds(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}

	holder, releaseHolder := openLockConn(t, dsn)
	require.NoError(t, waitForMigrationLock(context.Background(), holder, "postgres"),
		"the first caller should take the lock immediately")

	waiter, releaseWaiter := openLockConn(t, dsn)
	defer releaseWaiter()

	var (
		wg      sync.WaitGroup
		waitErr error
		done    = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitErr = waitForMigrationLock(context.Background(), waiter, "postgres")
		close(done)
	}()

	// Hold past a poll interval so the waiter is demonstrably blocked rather
	// than winning a race.
	select {
	case <-done:
		t.Fatal("waiter acquired the lock while it was still held")
	case <-time.After(2 * migrationLockPoll):
	}

	releaseHolder()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("waiter never acquired the lock after release")
	}
	wg.Wait()
	require.NoError(t, waitErr, "the waiter must acquire the lock, not fail")
}

func TestMigrationLock_UnknownEngineDoesNotBlock(t *testing.T) {
	locked, err := tryMigrationLock(context.Background(), nil, "cockroach")
	require.NoError(t, err)
	require.True(t, locked, "an engine with no advisory lock must proceed, not hang")
}
