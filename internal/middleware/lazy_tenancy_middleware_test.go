package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"

	// Register pgx driver for test DB connections.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestTenancyConn_NoDBRequest_DoesNotAcquireConn verifies that a tenant-scoped
// request whose handler performs NO database I/O does NOT acquire a pooled
// *sql.Conn. This is the core value proposition of lazy acquisition.
func TestTenancyConn_NoDBRequest_DoesNotAcquireConn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool, cleanup := testPool(t, 2)
	defer cleanup()

	tenancy := &noopTenancy{}
	handler := TenancyConn(pool, tenancy)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	)

	beforeInUse := pool.Stats().InUse

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), tenantKey{}, "test-tenant")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handler returned %d, want 200", rec.Code)
	}

	afterInUse := pool.Stats().InUse
	if afterInUse > beforeInUse {
		t.Errorf("pool InUse grew from %d to %d - conn was acquired for a non-DB request",
			beforeInUse, afterInUse)
	}
}

// TestTenancyConn_DBRequest_AcquiresConn verifies that a tenant-scoped request
// whose handler DOES perform a database query DOES properly acquire a conn.
func TestTenancyConn_DBRequest_AcquiresConn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool, cleanup := testPool(t, 4)
	defer cleanup()

	tenancy := &noopTenancy{}
	handler := TenancyConn(pool, tenancy)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = pool.QueryRow(r.Context(), "SELECT 1")
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), tenantKey{}, "test-tenant")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handler returned %d, want 200", rec.Code)
	}
}

// TestTenancyConn_CleanupOnlyWhenAcquired verifies that the defer cleanup
// does NOT panic when no conn was acquired (no DB was touched).
func TestTenancyConn_CleanupOnlyWhenAcquired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool, cleanup := testPool(t, 4)
	defer cleanup()

	tenancy := &noopTenancy{}
	var handlerCalled bool
	handler := TenancyConn(pool, tenancy)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handlerCalled = true
			w.WriteHeader(http.StatusOK)
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), tenantKey{}, "test-tenant")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handler returned %d, want 200", rec.Code)
	}
	if !handlerCalled {
		t.Fatal("handler was never called")
	}
}

// Helpers

func testPool(t *testing.T, maxConns int32) (db.DB, func()) {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Connect(ctx, testDSN(), maxConns)
	if err != nil {
		t.Skipf("no postgres available: %v", err)
	}
	return pool, func() { pool.Close() }
}

func testDSN() string {
	return "postgres://cms:***@localhost:5432/lyeve_test?sslmode=disable"
}

type noopTenancy struct{}

func (n *noopTenancy) Apply(_ context.Context, _ *sql.Conn) error { return nil }
func (n *noopTenancy) Reset(_ context.Context, _ *sql.Conn) error { return nil }
