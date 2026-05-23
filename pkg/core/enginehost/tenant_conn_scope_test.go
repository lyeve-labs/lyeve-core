//go:build !short && !mutest

package enginehost

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// AcquireTenantConn takes the tenant as an argument, but the tenancy strategies
// read it from the context, so the two can name different tenants and only one
// of them decides what the connection is scoped to.
//
// A caller can pass different values the two ways. The compliance export
// sweep does: it scopes the connection and stamps the tenant onto the context
// afterwards, so unless the argument decides, the sweep runs its exporters
// under one tenant on a connection pinned to another.
//
// MySQL is the dialect that makes the disagreement observable. USE changes
// DATABASE(), so the connection can be asked which tenant it is actually on.
func TestAcquireTenantConn_ScopesToTheArgumentNotTheContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is not mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	stamp := time.Now().UnixNano() % 100000
	asked := fmt.Sprintf("eh_asked_%d", stamp)
	onCtx := fmt.Sprintf("eh_onctx_%d", stamp)

	for _, slug := range []string{asked, onCtx} {
		name := sqlx.TenantSchemaName(slug)
		if _, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+name+"`"); err != nil {
			t.Fatalf("create database %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
		})
	}

	h := &engineHost{cfg: &config.Config{MultiTenant: true}, pool: pool}

	// The context names one tenant and the argument names another. The
	// argument is the one the caller asked for, so it is the one that has to
	// win.
	tctx, cleanup, err := h.AcquireTenantConn(core.WithTenantID(ctx, onCtx), asked)
	if err != nil {
		t.Fatalf("AcquireTenantConn: %v", err)
	}
	defer cleanup()

	conn := db.TenantConn(tctx)
	if conn == nil {
		t.Fatal("AcquireTenantConn returned a context with no tenant connection")
	}

	var got string
	if err := conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&got); err != nil {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}

	if want := sqlx.TenantSchemaName(asked); got != want {
		t.Fatalf("connection is scoped to %q, want %q: AcquireTenantConn applied the tenant from the context instead of the one it was asked for", got, want)
	}

	// The returned context has to agree with the connection, or the next
	// reader of the tenant id draws a different conclusion than the database
	// does.
	if got := core.TenantIDFromCtx(tctx); got != asked {
		t.Fatalf("returned context carries tenant %q, want %q", got, asked)
	}
}
