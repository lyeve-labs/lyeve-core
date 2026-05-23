//go:build !short && !mutest

package enginehost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// failingResetTenancy applies isolation normally and refuses to undo it. A
// reset fails for ordinary reasons (the server went away mid-request, the
// statement timed out), so this is not a hypothetical.
type failingResetTenancy struct{ inner db.Tenancy }

func (f failingResetTenancy) Apply(ctx context.Context, conn *sql.Conn) error {
	return f.inner.Apply(ctx, conn)
}

func (f failingResetTenancy) Reset(context.Context, *sql.Conn) error {
	return errors.New("reset failed")
}

// TestAcquireTenantConn_FailedResetDiscardsTheConnection is the gRPC-side twin
// of the HTTP middleware case.
//
// sql.Conn.Close hands the connection back to the pool with its session state
// intact, so a reset that did not happen leaves search_path still naming the
// tenant that just finished. With the pool capped at one connection, the next
// borrower is guaranteed to get that same connection.
func TestAcquireTenantConn_FailedResetDiscardsTheConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	slug := fmt.Sprintf("eh_reset_%d", time.Now().UnixNano()%100000)
	schema := "tenant_" + slug

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	sqlDB := pool.SQLDB()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	h := &engineHost{cfg: &config.Config{MultiTenant: true}, pool: pool}
	h.tenancyOnce.Do(func() {
		h.tenancyVal = failingResetTenancy{inner: db.NewPostgresSchemaTenancy(core.TenantIDFromCtx)}
	})

	tctx, cleanup, err := h.AcquireTenantConn(core.WithTenantID(ctx, slug), slug)
	if err != nil {
		t.Fatalf("AcquireTenantConn: %v", err)
	}
	if db.TenantConn(tctx) == nil {
		t.Fatal("no tenant connection was stored on the context")
	}
	cleanup()

	// Borrow straight from the pool, the way any untenanted caller does.
	var path string
	if err := sqlDB.QueryRowContext(ctx, "SHOW search_path").Scan(&path); err != nil {
		t.Fatalf("SHOW search_path: %v", err)
	}
	if strings.Contains(path, schema) {
		t.Fatalf("the pool handed back a connection still scoped to %s: search_path = %q", slug, path)
	}
}
