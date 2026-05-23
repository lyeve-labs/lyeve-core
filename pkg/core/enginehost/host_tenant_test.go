package enginehost

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// AcquireTenantConn: single-tenant pass-through

func TestAcquireTenantConn_SingleTenant_Noop(t *testing.T) {
	h := &engineHost{
		cfg:  &config.Config{MultiTenant: false},
		pool: &stubPool{err: errors.New("should not be called")},
	}

	ctx, cleanup, err := h.AcquireTenantConn(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	cleanup() // noop should not panic
	// noop path must return original ctx (not isolated).
	assert.Equal(t, context.Background().Value("tenant_id"), ctx.Value("tenant_id"), "noop should not inject tenant")
}

func TestAcquireTenantConn_EmptyTenant_Noop(t *testing.T) {
	h := &engineHost{
		cfg:  &config.Config{MultiTenant: true},
		pool: &stubPool{err: errors.New("should not be called")},
	}

	ctx, cleanup, err := h.AcquireTenantConn(context.Background(), "")
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	cleanup() // noop should not panic
	assert.Nil(t, db.TenantConn(ctx), "no tenant conn should be stored for empty tenant")
}

// AcquireTenantConn: conn acquire error

type stubPool struct {
	db.DB // embed so we only override Conn + Engine
	err   error
}

func (s *stubPool) Conn(ctx context.Context) (*sql.Conn, error) {
	return nil, s.err
}

func (s *stubPool) Engine() string { return "postgres" }

func TestAcquireTenantConn_ConnAcquireError(t *testing.T) {
	want := errors.New("connection refused")
	h := &engineHost{
		cfg:  &config.Config{MultiTenant: true},
		pool: &stubPool{err: want},
	}

	ctx, cleanup, err := h.AcquireTenantConn(context.Background(), "tenant-a")
	require.Error(t, err)
	assert.ErrorIs(t, err, want)
	assert.NotNil(t, cleanup)
	cleanup() // should not panic
	assert.Nil(t, db.TenantConn(ctx), "no tenant conn should be stored on error")
}

// AcquireTenantConn: real PG integration

type testPool struct {
	db  *sql.DB
	eng string
}

func (p *testPool) QueryRow(ctx context.Context, sql string, args ...any) (*sql.Row, error) {
	return p.db.QueryRowContext(ctx, sql, args...), nil
}
func (p *testPool) Query(ctx context.Context, sql string, args ...any) (*sql.Rows, error) {
	return p.db.QueryContext(ctx, sql, args...)
}
func (p *testPool) Exec(ctx context.Context, sql string, args ...any) (sql.Result, error) {
	return p.db.ExecContext(ctx, sql, args...)
}
func (p *testPool) Begin(ctx context.Context) (*sql.Tx, error) {
	return p.db.BeginTx(ctx, nil)
}
func (p *testPool) Conn(ctx context.Context) (*sql.Conn, error) {
	return p.db.Conn(ctx)
}
func (p *testPool) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}
func (p *testPool) Close() error {
	return p.db.Close()
}
func (p *testPool) Stats() sql.DBStats {
	return p.db.Stats()
}
func (p *testPool) Engine() string {
	if p.eng != "" {
		return p.eng
	}
	return "postgres"
}
func (p *testPool) SQLDB() *sql.DB { return p.db }
func (p *testPool) QuerierRO(ctx context.Context) (db.ReadOnlyQuerier, error) {
	return p, nil
}

func TestAcquireTenantConn_Postgres_AcquiresAndStoresConn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires running PG")
	}

	realDB, err := sql.Open("pgx", "postgres://lyeve:***@localhost:5432/lyeve?sslmode=disable")
	if err != nil {
		t.Skipf("no test PG: %v", err)
	}
	defer realDB.Close()

	if err := realDB.PingContext(context.Background()); err != nil {
		t.Skipf("PG ping failed: %v", err)
	}

	h := &engineHost{
		pool:  &testPool{db: realDB},
		rawDB: realDB,
		cfg:   &config.Config{MultiTenant: true},
	}

	tenantID := "test_acquire_conn"

	tenantCtx, cleanup, err := h.AcquireTenantConn(context.Background(), tenantID)
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	// Verify the conn was stowed on ctx.
	stored := db.TenantConn(tenantCtx)
	require.NotNil(t, stored, "tenant conn must be stored on ctx")

	// Verify we can query through the isolated conn.
	var result int
	err = stored.QueryRowContext(tenantCtx, "SELECT 1 AS v").Scan(&result)
	require.NoError(t, err)
	assert.Equal(t, 1, result)

	cleanup()

	// After cleanup, the conn should be closed.
	if stored := db.TenantConn(tenantCtx); stored != nil {
		var x int
		err = stored.QueryRowContext(context.Background(), "SELECT 1").Scan(&x)
		assert.Error(t, err, "conn should be closed after cleanup")
	}
}

// AcquireTenantConn: single-tenant integration

func TestAcquireTenantConn_SingleTenant_PassThroughWithConn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires running PG")
	}

	realDB, err := sql.Open("pgx", "postgres://lyeve:***@localhost:5432/lyeve?sslmode=disable")
	if err != nil {
		t.Skipf("no test PG: %v", err)
	}
	defer realDB.Close()
	if err := realDB.PingContext(context.Background()); err != nil {
		t.Skipf("PG ping failed: %v", err)
	}

	// MultiTenant=false even with a valid pool: must not acquire a conn.
	h := &engineHost{
		pool:  &testPool{db: realDB},
		rawDB: realDB,
		cfg:   &config.Config{MultiTenant: false},
	}

	ctx, cleanup, err := h.AcquireTenantConn(context.Background(), "some-tenant")
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	cleanup()

	// No tenant conn should be on ctx.
	stored := db.TenantConn(ctx)
	assert.Nil(t, stored, "no tenant conn should be stored in single-tenant mode")
}
