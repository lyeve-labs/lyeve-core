//go:build !short && !mutest

package middleware_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// MySQL: dual-tenant isolation

func TestTenancyConn_DualTenant_MySQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is not mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	tenantA := fmt.Sprintf("dual_a_%d", ts%10000)
	tenantB := fmt.Sprintf("dual_b_%d", ts%10000)
	dbA := "tenant_" + tenantA
	dbB := "tenant_" + tenantB

	// Create both tenant databases + tables.
	_, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbA+"`")
	if err != nil {
		t.Fatalf("create db A: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbB+"`")
	if err != nil {
		t.Fatalf("create db B: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS `"+dbA+"`.items (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(255))")
	if err != nil {
		t.Fatalf("create table A: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS `"+dbB+"`.items (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(255))")
	if err != nil {
		t.Fatalf("create table B: %v", err)
	}
	_, _ = pool.Exec(ctx, "INSERT INTO `"+dbA+"`.items (label) VALUES (?)", "data-"+tenantA)
	_, _ = pool.Exec(ctx, "INSERT INTO `"+dbB+"`.items (label) VALUES (?)", "data-"+tenantB)

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+dbA+"`")
		pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+dbB+"`")
	})

	tenancy := db.NewMySQLDatabaseTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	}, "lyeve_test")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var count int
		row, qrErr := pool.QueryRow(r.Context(),
			"SELECT COUNT(*) FROM items WHERE label LIKE ?",
			"data-"+mw.TenantIDFromCtx(r),
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Errorf("tenant %q: query error: %v", mw.TenantIDFromCtx(r), err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if count != 1 {
			t.Errorf("tenant %q: expected 1 row, got %d (cross-tenant leak!)", mw.TenantIDFromCtx(r), count)
		}
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// Tenant A
	reqA := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claimsA := &auth.Claims{TenantID: tenantA, Roles: []string{"admin"}}
	reqA = reqA.WithContext(context.WithValue(reqA.Context(), auth.ClaimsKey, claimsA))
	wA := httptest.NewRecorder()
	m.ServeHTTP(wA, reqA)
	if wA.Code != http.StatusOK {
		t.Errorf("tenant A: status %d, body %s", wA.Code, wA.Body.String())
	}

	// Tenant B
	reqB := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claimsB := &auth.Claims{TenantID: tenantB, Roles: []string{"admin"}}
	reqB = reqB.WithContext(context.WithValue(reqB.Context(), auth.ClaimsKey, claimsB))
	wB := httptest.NewRecorder()
	m.ServeHTTP(wB, reqB)
	if wB.Code != http.StatusOK {
		t.Errorf("tenant B: status %d, body %s", wB.Code, wB.Body.String())
	}
}

// MySQL: tenant lifecycle (create -> seed -> verify -> drop -> gone)

func TestTenancyConn_MySQL_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is not mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	slug := fmt.Sprintf("lfc_%d", time.Now().UnixNano()%10000)
	dbName := "tenant_" + slug

	// Phase 1: Create
	_, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbName+"`")
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS `"+dbName+"`.lfc (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(100))")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Phase 2: Seed
	_, err = pool.Exec(ctx, "INSERT INTO `"+dbName+"`.lfc (name) VALUES ('item-a'),('item-b'),('item-c')")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Phase 3: Verify through middleware
	tenancy := db.NewMySQLDatabaseTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	}, "lyeve_test")

	var names []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), "SELECT name FROM lfc ORDER BY id")
		if err != nil {
			t.Errorf("query: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/lfc", nil)
	claims := &auth.Claims{TenantID: slug, Roles: []string{"admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("phase 3: status %d", w.Code)
	}
	if len(names) != 3 {
		t.Errorf("phase 3: expected 3 items, got %d: %v", len(names), names)
	}

	// Phase 4: Drop
	_, err = pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
	if err != nil {
		t.Fatalf("drop database: %v", err)
	}

	// Phase 5: Verify gone
	var dbCount int
	row, qrErr := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", dbName,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&dbCount)
	if err != nil {
		t.Fatalf("check database existence: %v", err)
	}
	if dbCount != 0 {
		t.Errorf("phase 5: database %s still exists after DROP", dbName)
	}
}

// MySQL: invalid slug through middleware

func TestTenancyConn_MySQL_InvalidSlug_HTTP500(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is not mysql")
	}

	pool := testdb.MySQL(t)
	tenancy := db.NewMySQLDatabaseTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	}, "lyeve_test")

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Lazy tenancy applies isolation on first DB use. An invalid slug is
		// rejected by Apply at acquire time, which a handler maps to 500.
		if _, err := db.LazyTenantConnFromCtx(r.Context()).Acquire(r.Context()); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{TenantID: "BAD_SLUG", Roles: []string{"admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("invalid slug MySQL: expected 500, got %d", w.Code)
	}
}

// MySQL: query through tenancy without tenant, passthrough

func TestTenancyConn_MySQL_NoTenant_Passthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is not mysql")
	}

	pool := testdb.MySQL(t)
	tenancy := db.NewMySQLDatabaseTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	}, "lyeve_test")

	var called bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// No JWT claims on context.
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if !called {
		t.Error("handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}
