package testhost

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Fake pool
// fakeDB implements db.DB for testing testhost without real database connections.

var _ db.DB = (*fakeDB)(nil)

type fakeDB struct {
	engine     string
	sqlDB      *sql.DB
	closeFn    func() error
	queryRowFn func(ctx context.Context, sql string, args ...any) (*sql.Row, error)
	queryFn    func(ctx context.Context, sql string, args ...any) (*sql.Rows, error)
	execFn     func(ctx context.Context, sql string, args ...any) (sql.Result, error)
	beginFn    func(ctx context.Context) (*sql.Tx, error)
}

func (f *fakeDB) Engine() string { return f.engine }
func (f *fakeDB) SQLDB() *sql.DB { return f.sqlDB }

func (f *fakeDB) QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error) {
	if f.queryRowFn != nil {
		return f.queryRowFn(ctx, sql, args...)
	}
	return nil, nil
}
func (f *fakeDB) Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	if f.queryFn != nil {
		return f.queryFn(ctx, sql, args...)
	}
	return nil, nil
}
func (f *fakeDB) Exec(ctx context.Context, sql string, args ...any) (sql.Result, error) {
	if f.execFn != nil {
		return f.execFn(ctx, sql, args...)
	}
	return nil, nil
}
func (f *fakeDB) Begin(ctx context.Context) (*sql.Tx, error) {
	if f.beginFn != nil {
		return f.beginFn(ctx)
	}
	return nil, nil
}
func (f *fakeDB) Conn(ctx context.Context) (*sql.Conn, error) { return nil, nil }
func (f *fakeDB) Ping(ctx context.Context) error              { return nil }
func (f *fakeDB) Close() error {
	if f.closeFn != nil {
		return f.closeFn()
	}
	return nil
}
func (f *fakeDB) Stats() sql.DBStats { return sql.DBStats{} }
func (f *fakeDB) QuerierRO(ctx context.Context) (db.ReadOnlyQuerier, error) {
	return &readOnlyWrapper{pool: f}, nil
}

// stubResult implements sql.Result for testing.
type stubResult struct{}

func (s stubResult) LastInsertId() (int64, error) { return 0, nil }
func (s stubResult) RowsAffected() (int64, error) { return 1, nil }

// readOnlyWrapper wraps fakeDB as a ReadOnlyQuerier.
type readOnlyWrapper struct{ pool *fakeDB }

func (r *readOnlyWrapper) QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error) {
	return r.pool.QueryRow(ctx, sql, args...)
}
func (r *readOnlyWrapper) Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	return r.pool.Query(ctx, sql, args...)
}

// New()

func TestNew_StoresPoolAndConfig(t *testing.T) {
	pool := &fakeDB{engine: "postgres"}
	host := New(pool)

	assert.Same(t, pool, host.Pool(), "Pool() should return the injected pool")
	assert.NotNil(t, host.Config(), "Config() should return a non-nil Config")
	assert.Equal(t, "test", host.Version(), "Version() should return \"test\"")
}

func TestNew_ConfigDefaults(t *testing.T) {
	pool := &fakeDB{engine: "postgres"}
	host := New(pool)

	instanceID := host.Config().String("instance_id")
	assert.Equal(t, "test", instanceID, "InstanceID should be \"test\"")

	cfgStore := host.cfg
	require.NotNil(t, cfgStore, "cfg should be set after New()")
	assert.Equal(t, 30*time.Second, cfgStore.GracefulShutdownTimeout,
		"GracefulShutdownTimeout should be 30s")
}

func TestNew_DialectFromPool(t *testing.T) {
	tests := []struct {
		name   string
		engine string
		want   string
	}{
		{name: "postgres", engine: "postgres", want: "postgres"},
		{name: "mysql", engine: "mysql", want: "mysql"},
		{name: "mssql", engine: "mssql", want: "mssql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := New(&fakeDB{engine: tt.engine})
			assert.Equal(t, tt.want, host.Dialect())
		})
	}
}

func TestNew_RawDBFromPool(t *testing.T) {
	db1, err := sql.Open("postgres", "") // driver-less handle
	if err != nil {
		t.Fatalf("sql.Open failed (expected): %v", err)
	}
	defer db1.Close()

	pool := &fakeDB{engine: "postgres", sqlDB: db1}
	host := New(pool)

	assert.Same(t, db1, host.RawDB(), "RawDB() should return pool.SQLDB()")
}

func TestNew_NilSQLDB(t *testing.T) {
	pool := &fakeDB{engine: "postgres", sqlDB: nil}
	host := New(pool)

	assert.Nil(t, host.RawDB(), "RawDB() should return nil when pool.SQLDB() is nil")
}

// Querier()

func TestQuerier_ReturnsNonNil(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	q := host.Querier(context.Background())
	assert.NotNil(t, q, "Querier() should return a non-nil Querier")
}

func TestQuerier_DelegatesQueryRow(t *testing.T) {
	var called bool
	pool := &fakeDB{
		engine: "postgres",
		queryRowFn: func(_ context.Context, sql string, _ ...any) (*sql.Row, error) {
			called = true
			assert.Equal(t, "SELECT 1", sql)
			return nil, nil
		},
	}
	host := New(pool)
	q := host.Querier(context.Background())
	_, _ = q.QueryRow(context.Background(), "SELECT 1")
	assert.True(t, called, "Querier().QueryRow should delegate to pool.QueryRow")
}

func TestQuerier_DelegatesQuery(t *testing.T) {
	var called bool
	pool := &fakeDB{
		engine: "postgres",
		queryFn: func(_ context.Context, sql string, _ ...any) (*sql.Rows, error) {
			called = true
			assert.Equal(t, "SELECT * FROM test", sql)
			return nil, nil
		},
	}
	host := New(pool)
	q := host.Querier(context.Background())
	_, _ = q.Query(context.Background(), "SELECT * FROM test")
	assert.True(t, called, "Querier().Query should delegate to pool.Query")
}

func TestQuerier_DelegatesExec(t *testing.T) {
	var called bool
	pool := &fakeDB{
		engine: "postgres",
		execFn: func(_ context.Context, sql string, _ ...any) (sql.Result, error) {
			called = true
			assert.Equal(t, "INSERT INTO test (id) VALUES ($1)", sql)
			return stubResult{}, nil
		},
	}
	host := New(pool)
	q := host.Querier(context.Background())
	_, _ = q.Exec(context.Background(), "INSERT INTO test (id) VALUES ($1)")
	assert.True(t, called, "Querier().Exec should delegate to pool.Exec")
}

// Logger()

func TestLogger_ReturnsDefault(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	logger := host.Logger(context.Background())
	assert.NotNil(t, logger, "Logger() should return a non-nil *slog.Logger")
	assert.Equal(t, slog.Default(), logger, "Logger() should return slog.Default()")
}

// Config()

func TestConfig_ReturnsNonNil(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	cfg := host.Config()
	assert.NotNil(t, cfg, "Config() should return non-nil")
}

func TestConfig_ReadableValues(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	cfg := host.Config()

	// InstanceID is readable and redacted fields return "".
	assert.Equal(t, "test", cfg.String("instance_id"),
		"Config should expose instance_id = \"test\"")
	assert.Equal(t, "", cfg.String("database_url"),
		"database_url should be redacted/empty in test config")
}

func TestConfig_BoolDefault(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	cfg := host.Config()

	assert.False(t, cfg.Bool("nonexistent"),
		"Bool() on missing key should return false")
}

func TestConfig_DurationDefault(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	cfg := host.Config()

	assert.Equal(t, time.Duration(0), cfg.Duration("nonexistent"),
		"Duration() on missing key should return 0")
}

func TestConfig_StringsDefault(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	cfg := host.Config()

	assert.Nil(t, cfg.Strings("nonexistent"),
		"Strings() on missing key should return nil")
}

// Hooks()

func TestHooks_ReturnsNonNil(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	hookBus := host.Hooks()
	assert.NotNil(t, hookBus, "Hooks() should return a non-nil HookBus")
}

func TestHooks_SubscribePanicsOnNilRegistry(t *testing.T) {
	// New() passes nil registry. Subscribe on a nil *hooks.Registry panics
	// because hookBusAdapter.Subscribe calls h.registry.RegisterDynamic.
	host := New(&fakeDB{engine: "postgres"})
	hookBus := host.Hooks()

	assert.Panics(t, func() {
		hookBus.Subscribe("test_schema", core.BeforeCreate,
			func(_ context.Context, _ core.Event) error { return nil })
	}, "Subscribe on nil-registry test host should panic")
}

// Version()

func TestVersion_ReturnsTest(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	assert.Equal(t, "test", host.Version())
}

// Schema()

func TestSchema_ReturnsNil(t *testing.T) {
	host := New(&fakeDB{engine: "postgres"})
	assert.Nil(t, host.Schema(), "Schema() should return nil (no schema engine wired)")
}

// Pool()

func TestPool_ReturnsInjectedPool(t *testing.T) {
	pool := &fakeDB{engine: "mssql"}
	host := New(pool)
	assert.Same(t, pool, host.Pool(), "Pool() should return the pool passed to New()")
}

// Close()

func TestClose_DelegatesToPool(t *testing.T) {
	var closeCalled bool
	pool := &fakeDB{
		engine:  "postgres",
		closeFn: func() error { closeCalled = true; return nil },
	}
	host := New(pool)

	err := host.Close()
	assert.NoError(t, err, "Close() should not error")
	assert.True(t, closeCalled, "Close() should delegate to pool.Close()")
}

func TestClose_PropagatesPoolError(t *testing.T) {
	wantErr := errors.New("pool close failed")
	pool := &fakeDB{
		engine:  "postgres",
		closeFn: func() error { return wantErr },
	}
	host := New(pool)

	err := host.Close()
	assert.ErrorIs(t, err, wantErr, "Close() should propagate pool.Close() error")
}

// Multiple instances (no shared state)

func TestMultipleInstances_IndependentState(t *testing.T) {
	dbA := &sql.DB{}
	dbB := &sql.DB{}
	poolA := &fakeDB{engine: "postgres", sqlDB: dbA}
	poolB := &fakeDB{engine: "mysql", sqlDB: dbB}

	hostA := New(poolA)
	hostB := New(poolB)

	assert.Equal(t, "postgres", hostA.Dialect(), "hostA dialect should be postgres")
	assert.Equal(t, "mysql", hostB.Dialect(), "hostB dialect should be mysql")
	assert.NotSame(t, hostA.Pool(), hostB.Pool(), "each host should have its own pool")
	assert.NotSame(t, hostA.RawDB(), hostB.RawDB(), "each host should have its own rawDB")
	assert.NotSame(t, hostA.Config(), hostB.Config(), "each host should have its own config")
}

// Concurrent safety (no data races)

func TestConcurrentAccess_RaceFree(t *testing.T) {
	pool := &fakeDB{
		engine:     "postgres",
		queryRowFn: func(_ context.Context, _ string, _ ...any) (*sql.Row, error) { return nil, nil },
		queryFn:    func(_ context.Context, _ string, _ ...any) (*sql.Rows, error) { return nil, nil },
		execFn:     func(_ context.Context, _ string, _ ...any) (sql.Result, error) { return stubResult{}, nil },
		closeFn:    func() error { return nil },
	}
	host := New(pool)
	ctx := context.Background()

	concurrent := func(fn func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			fn()
		}()
		fn()
		<-done
	}

	concurrent(func() { _ = host.Dialect() })
	concurrent(func() { _ = host.Version() })
	concurrent(func() { _ = host.Logger(ctx) })
	concurrent(func() { _ = host.Pool() })
	concurrent(func() { _ = host.Schema() })
	concurrent(func() { _ = host.Config() })
	concurrent(func() { _ = host.Hooks() })
	concurrent(func() {
		q := host.Querier(ctx)
		_, _ = q.QueryRow(ctx, "SELECT 1")
	})
	concurrent(func() {
		q := host.Querier(ctx)
		_, _ = q.Query(ctx, "SELECT 1")
	})
	concurrent(func() {
		q := host.Querier(ctx)
		_, _ = q.Exec(ctx, "SELECT 1")
	})
}
