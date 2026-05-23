//go:build !mutest

package db_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// LazyTenantConn integration tests (needs a real *sql.DB)

func TestLazyTenantConn_Acquire_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	lt := db.NewLazyTenantConn(pool.SQLDB(), &noopTenancy{})
	conn, err := lt.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if conn == nil {
		t.Fatal("Acquire returned nil conn")
	}
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestLazyTenantConn_Acquire_Idempotent_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	lt := db.NewLazyTenantConn(pool.SQLDB(), &noopTenancy{})
	c1, err := lt.Acquire(ctx)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	c2, err := lt.Acquire(ctx)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if c1 != c2 {
		t.Error("Acquire should return the same *sql.Conn on repeated calls")
	}
}

func TestLazyTenantConn_Acquired_AfterAcquire_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	lt := db.NewLazyTenantConn(pool.SQLDB(), &noopTenancy{})
	conn, err := lt.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := lt.Acquired(); c != conn {
		t.Error("Acquired() should return the same conn after Acquire()")
	}
}

func TestLazyTenantConn_ConcurrentAcquire_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	lt := db.NewLazyTenantConn(pool.SQLDB(), &noopTenancy{})

	var wg sync.WaitGroup
	results := make([]*sql.Conn, 10)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c, err := lt.Acquire(ctx)
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			results[idx] = c
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, c := range results {
		if c != first {
			t.Errorf("goroutine %d got different conn", i)
		}
	}
}

// DB routing through lazy context

func TestDB_RoutesThroughLazyConn_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	// Create a LazyTenantConn and put it on ctx.
	lt := db.NewLazyTenantConn(pool.SQLDB(), &noopTenancy{})
	ctx = db.WithLazyTenantConn(ctx, lt)

	// A query through the DB interface should trigger lazy acquire and
	// route through the acquired conn.
	_, _ = pool.QueryRow(ctx, "SELECT 1")
	// QueryRow returns *sql.Row: no error until Scan. But if the lazy
	// acquire failed, the row would carry a context error.

	// Verify the conn was acquired.
	if c := lt.Acquired(); c == nil {
		t.Error("Acquired() should return non-nil after a DB query through lazy context")
	}
}

// noopTenancy

type noopTenancy struct{}

func (n *noopTenancy) Apply(_ context.Context, _ *sql.Conn) error { return nil }
func (n *noopTenancy) Reset(_ context.Context, _ *sql.Conn) error { return nil }
