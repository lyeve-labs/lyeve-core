//go:build !short && !mutest

package middleware_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// failingResetTenancy applies isolation normally and refuses to undo it. A
// reset can fail for ordinary reasons (the server went away mid-request, the
// statement timed out), so this is not a hypothetical.
type failingResetTenancy struct {
	inner db.Tenancy
}

func (f failingResetTenancy) Apply(ctx context.Context, conn *sql.Conn) error {
	return f.inner.Apply(ctx, conn)
}

func (f failingResetTenancy) Reset(context.Context, *sql.Conn) error {
	return errors.New("reset failed")
}

// TestTenancyConn_FailedResetDoesNotReturnTheConnectionToThePool pins the
// consequence of a reset that did not happen.
//
// sql.Conn.Close does not close anything: it hands the connection back to the
// pool with its session state intact. With the pool capped at one connection,
// the second request is guaranteed to get the same one, so if the search_path
// survived, the second request reads the first tenant's schema while believing
// it is unscoped.
func TestTenancyConn_FailedResetDoesNotReturnTheConnectionToThePool(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	slug := fmt.Sprintf("mw_reset_%d", time.Now().UnixNano()%100000)
	schema := "tenant_" + slug

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	// One connection, so the second request cannot avoid reusing the first's.
	sqlDB := pool.SQLDB()
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	tenancy := failingResetTenancy{inner: db.NewPostgresSchemaTenancy(
		func(ctx context.Context) string { return mw.TenantIDFromContext(ctx) })}

	// The tenanted request only has to touch the DB, so a connection is
	// acquired and the isolation is applied to it.
	tenanted := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var one int
			row, err := pool.QueryRow(r.Context(), "SELECT 1")
			if err != nil {
				t.Fatalf("QueryRow: %v", err)
			}
			if err := row.Scan(&one); err != nil {
				t.Fatalf("scan: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		})))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req = req.WithContext(context.WithValue(req.Context(),
		auth.ClaimsKey, &auth.Claims{TenantID: slug, Roles: []string{"admin"}}))
	rec := httptest.NewRecorder()
	tenanted.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("tenanted request: status %d, want 200", rec.Code)
	}

	// Now borrow straight from the pool, the way any untenanted caller does.
	// A connection that kept the failed reset's search_path answers with the
	// tenant schema. A connection that was discarded answers with the default.
	var path string
	if err := sqlDB.QueryRowContext(ctx, "SHOW search_path").Scan(&path); err != nil {
		t.Fatalf("SHOW search_path: %v", err)
	}
	// Postgres renders the path unquoted when the name needs no quoting, so
	// match on the schema appearing at all rather than on an exact rendering.
	if strings.Contains(path, schema) {
		t.Fatalf("the pool handed back a connection still scoped to %s: search_path = %q", slug, path)
	}
}
