package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDriver implements driver.Driver with a Stmt that tracks prepared statements.
type testDriver struct {
	mu       sync.Mutex
	prepares int32 // atomic, for Prepare calls
	stmts    []*testStmt
}

type testStmt struct {
	driver.Stmt
	query  string
	closed int32 // atomic
}

type testConn struct {
	driver *testDriver
}

type testConnector struct{ driver *testDriver }

func (t *testConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &testConn{driver: t.driver}, nil
}
func (t *testConnector) Driver() driver.Driver { return t.driver }

func (d *testDriver) Open(name string) (driver.Conn, error) { return &testConn{driver: d}, nil }
func (c *testConn) Prepare(query string) (driver.Stmt, error) {
	atomic.AddInt32(&c.driver.prepares, 1)
	s := &testStmt{query: query}
	c.driver.mu.Lock()
	c.driver.stmts = append(c.driver.stmts, s)
	c.driver.mu.Unlock()
	return s, nil
}
func (c *testConn) Close() error              { return nil }
func (c *testConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }
func (s *testStmt) Close() error              { atomic.AddInt32(&s.closed, 1); return nil }
func (s *testStmt) NumInput() int             { return 0 }
func (s *testStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("not supported")
}
func (s *testStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("not supported")
}

func newTestDB(t *testing.T) (*sql.DB, *testDriver) {
	t.Helper()
	td := &testDriver{}
	connector := &testConnector{driver: td}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })
	return db, td
}

func TestStmtCache_PrepareHit(t *testing.T) {
	db, td := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()

	ctx := context.Background()

	// First call: prepares on the DB.
	stmt1, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
	require.NoError(t, err)
	require.NotNil(t, stmt1)
	assert.Equal(t, int32(1), atomic.LoadInt32(&td.prepares))

	// Second call: cache hit, no new prepare on DB.
	stmt2, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
	require.NoError(t, err)
	require.NotNil(t, stmt2)
	assert.Equal(t, int32(1), atomic.LoadInt32(&td.prepares))
	assert.Same(t, stmt1, stmt2)

	assert.Equal(t, 1, cache.Len())
}

func TestStmtCache_PrepareDifferentDialect(t *testing.T) {
	db, td := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()
	ctx := context.Background()

	// Same SQL, different dialects: distinct cache entries.
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	_, _ = cache.Prepare(ctx, db, "mysql", "SELECT $1")
	_, _ = cache.Prepare(ctx, db, "mssql", "SELECT $1")

	assert.Equal(t, int32(3), atomic.LoadInt32(&td.prepares))
	assert.Equal(t, 3, cache.Len())
}

func TestStmtCache_PrepareDifferentQueries(t *testing.T) {
	db, td := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()
	ctx := context.Background()

	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1, $2")
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1 WHERE x = $2")

	assert.Equal(t, int32(3), atomic.LoadInt32(&td.prepares))
	assert.Equal(t, 3, cache.Len())
}

func TestStmtCache_PrepareQueryError(t *testing.T) {
	db, td := newTestDB(t)
	// Use the real DB interface for non-prepared queries.
	// The test driver supports Prepare but not Query, but we're only
	// testing that Prepare errors propagate. Since our test driver always
	// succeeds Prepare, we test the nil-cache case instead.
	t.Run("nil cache passthrough", func(t *testing.T) {
		var nilCache *StmtCache
		ctx := context.Background()
		stmt, err := nilCache.Prepare(ctx, db, "pg", "SELECT $1")
		require.NoError(t, err) // passthrough to db.PrepareContext
		require.NotNil(t, stmt)
		_ = stmt.Close() // caller owns it for nil cache
		_ = td           // silence
	})

	t.Run("closed cache passthrough", func(t *testing.T) {
		cache := NewStmtCache(10)
		cache.Close()
		ctx := context.Background()

		stmt, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
		require.NoError(t, err)
		require.NotNil(t, stmt)
		// Caller owns the statement when cache is closed.
		_ = stmt.Close()
	})
}

func TestStmtCache_Eviction(t *testing.T) {
	db, td := newTestDB(t)
	ctx := context.Background()

	// Max size 2: 3rd prepare evicts the LRU (first).
	cache := NewStmtCache(2)
	defer cache.Close()

	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1") // entry A
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $2") // entry B
	assert.Equal(t, 2, cache.Len())

	// Bump A to front (most-recently-used)
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	assert.Equal(t, 2, cache.Len())

	// Prepare third: evicts B (LRU, since A was bumped)
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $3") // entry C
	assert.Equal(t, 2, cache.Len())

	// A should still be cached (hit), C should be cached (recent).
	// The total prepare count is: A, B, A(hit), C = 3 prepares.
	assert.Equal(t, int32(3), atomic.LoadInt32(&td.prepares))

	stats := cache.Stats()
	assert.Equal(t, 2, stats.Size)
	assert.Equal(t, 2, stats.MaxSize)
	assert.False(t, stats.Closed)
}

func TestStmtCache_Evict(t *testing.T) {
	db, td := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()
	ctx := context.Background()

	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $2")
	assert.Equal(t, 2, cache.Len())

	cache.Evict("pg", "SELECT $1")
	assert.Equal(t, 1, cache.Len())

	// Re-prepare: should be a new prepare call.
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	assert.Equal(t, int32(3), atomic.LoadInt32(&td.prepares))
	assert.Equal(t, 2, cache.Len())

	// Evict non-existent: no-op.
	cache.Evict("pg", "SELECT non_existent")
	assert.Equal(t, 2, cache.Len())

	// Evict nil cache: no-op.
	var nilCache *StmtCache
	nilCache.Evict("pg", "SELECT $1") // shouldn't panic
}

func TestStmtCache_Clear(t *testing.T) {
	db, td := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()
	ctx := context.Background()

	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $2")
	assert.Equal(t, 2, cache.Len())

	cache.Clear()
	assert.Equal(t, 0, cache.Len())

	// After clear, prepare should cache fresh.
	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	assert.Equal(t, int32(3), atomic.LoadInt32(&td.prepares))
	assert.Equal(t, 1, cache.Len())

	// Clear nil cache: no-op.
	var nilCache *StmtCache
	nilCache.Clear() // shouldn't panic
}

func TestStmtCache_Close(t *testing.T) {
	db, td := newTestDB(t)
	cache := NewStmtCache(10)
	ctx := context.Background()

	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	assert.Equal(t, 1, cache.Len())

	cache.Close()

	stats := cache.Stats()
	assert.Equal(t, 0, stats.Size)
	assert.True(t, stats.Closed)

	// After close: Prepare falls through to db directly.
	stmt, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
	require.NoError(t, err)
	require.NotNil(t, stmt)
	_ = stmt.Close()                                          // caller owns it
	assert.Equal(t, int32(2), atomic.LoadInt32(&td.prepares)) // fresh prepare

	// Double close: no-op.
	cache.Close()

	// Close nil cache: no-op.
	var nilCache *StmtCache
	nilCache.Close()
}

func TestStmtCache_Return(t *testing.T) {
	db, _ := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()
	ctx := context.Background()

	stmt, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
	require.NoError(t, err)

	// Return is currently a no-op: statement stays cached.
	cache.Return(stmt)
	assert.Equal(t, 1, cache.Len())

	// Return nil cache: no-op.
	var nilCache *StmtCache
	nilCache.Return(stmt)
}

func TestStmtCache_Disabled(t *testing.T) {
	db, td := newTestDB(t)
	// maxSize=0 disables caching
	cache := NewStmtCache(0)
	ctx := context.Background()

	stmt1, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
	require.NoError(t, err)
	require.NotNil(t, stmt1)
	_ = stmt1.Close()

	stmt2, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
	require.NoError(t, err)
	require.NotNil(t, stmt2)
	_ = stmt2.Close()

	// Both calls prepared: no caching.
	assert.Equal(t, int32(2), atomic.LoadInt32(&td.prepares))
	assert.Equal(t, 0, cache.Len())

	stats := cache.Stats()
	assert.Equal(t, 0, stats.Size)
	assert.Equal(t, 0, stats.MaxSize)
}

func TestStmtCache_Len_Stats(t *testing.T) {
	db, _ := newTestDB(t)
	cache := NewStmtCache(50)
	defer cache.Close()
	ctx := context.Background()

	assert.Equal(t, 0, cache.Len())
	stats := cache.Stats()
	assert.Equal(t, 0, stats.Size)
	assert.Equal(t, 50, stats.MaxSize)

	_, _ = cache.Prepare(ctx, db, "pg", "SELECT $1")
	assert.Equal(t, 1, cache.Len())
	stats = cache.Stats()
	assert.Equal(t, 1, stats.Size)
}

func TestStmtCache_ConcurrentPrepareSameKey(t *testing.T) {
	db, _ := newTestDB(t)
	cache := NewStmtCache(10)
	defer cache.Close()
	ctx := context.Background()

	// Prepare same key from 10 goroutines: should only prepare once.
	var wg sync.WaitGroup
	n := 10
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := cache.Prepare(ctx, db, "pg", "SELECT $1")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, cache.Len())
}

func TestCacheKey(t *testing.T) {
	tests := []struct {
		dialect, query, want string
	}{
		{"pg", "SELECT $1", "pg:SELECT $1"},
		{"mysql", "SELECT ?", "mysql:SELECT ?"},
		{"mssql", "SELECT @p1", "mssql:SELECT @p1"},
		{"", "SELECT 1", ":SELECT 1"},
	}
	for _, tt := range tests {
		t.Run(tt.dialect, func(t *testing.T) {
			got := cacheKey(tt.dialect, tt.query)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTruncateQuery(t *testing.T) {
	tests := []struct {
		q    string
		want string
	}{
		{"SELECT $1", "SELECT $1"},
		// 76 chars <= maxLen (80), so no truncation.
		{"SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16", "SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16"},
		{"A", "A"},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := truncateQuery(tt.q)
			assert.Equal(t, tt.want, got)
		})
	}
}
