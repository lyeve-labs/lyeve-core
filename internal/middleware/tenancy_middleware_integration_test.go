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

// TenancyConn integration: Postgres schema-per-tenant

// TestTenancyConn_Postgres_Isolation verifies the full middleware pipeline
// (TenantHeader -> TenancyConn -> handler) routes a request through a
// tenant-isolated connection, confirming queries execute in the correct
// Postgres schema.
func TestTenancyConn_Postgres_Isolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	slug := fmt.Sprintf("mw_pg_%d", time.Now().UnixNano()%10000)
	schema := "tenant_" + slug

	// Provision tenant schema + table.
	_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+schema+".mw_items (id SERIAL PRIMARY KEY, label TEXT)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	// Insert a row directly so the tenant query can find it.
	_, err = pool.Exec(ctx, "INSERT INTO "+schema+".mw_items (label) VALUES ($1)", "tenanted-data")
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}

	// Build full middleware stack: TenantHeader -> TenancyConn -> handler.
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var lbl string
		row, qrErr := pool.QueryRow(r.Context(), "SELECT label FROM mw_items WHERE label = $1", "tenanted-data")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		if err := row.Scan(&lbl); err != nil {
			t.Logf("tenant-scoped query: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if lbl != "tenanted-data" {
			t.Errorf("label = %q, want 'tenanted-data'", lbl)
		}
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// Simulate JWT with tenant claim (as TenantHeader would see).
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{
		TenantID: slug,
		Roles:    []string{"admin"},
	}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		body := w.Body.String()
		t.Errorf("status = %d, want 200; body: %s", w.Code, body)
	}
}

// TenancyConn integration: MySQL database-per-tenant

// TestTenancyConn_MySQL_Isolation verifies the full middleware pipeline with
// MySQL database-per-tenant isolation.
func TestTenancyConn_MySQL_Isolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT is not mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	slug := fmt.Sprintf("mw_my_%d", time.Now().UnixNano()%10000)
	dbName := "tenant_" + slug

	// Provision tenant database + table.
	_, err := pool.Exec(ctx, "CREATE DATABASE IF NOT EXISTS `"+dbName+"`")
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS `"+dbName+"`.mw_items (id INT AUTO_INCREMENT PRIMARY KEY, label VARCHAR(255))")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO `"+dbName+"`.mw_items (label) VALUES ('tenanted-mysql-data')")
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
	})

	tenancy := db.NewMySQLDatabaseTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	}, "lyeve_test")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var lbl string
		row, qrErr := pool.QueryRow(r.Context(), "SELECT label FROM mw_items WHERE label = $1", "tenanted-mysql-data")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		if err := row.Scan(&lbl); err != nil {
			t.Logf("tenant-scoped query: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if lbl != "tenanted-mysql-data" {
			t.Errorf("label = %q, want 'tenanted-mysql-data'", lbl)
		}
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{
		TenantID: slug,
		Roles:    []string{"admin"},
	}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		body := w.Body.String()
		t.Errorf("status = %d, want 200; body: %s", w.Code, body)
	}
}

// TenancyConn integration: MSSQL database-per-tenant

// TestTenancyConn_MSSQL_Isolation verifies the full middleware pipeline with
// MSSQL database-per-tenant isolation.
func TestTenancyConn_MSSQL_Isolation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT is not mssql")
	}

	pool := testdb.MSSQL(t)
	ctx := context.Background()

	slug := fmt.Sprintf("mw_ms_%d", time.Now().UnixNano()%10000)
	dbName := "tenant_" + slug

	// Provision tenant database + table.
	_, err := pool.Exec(ctx, "CREATE DATABASE ["+dbName+"]")
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE ["+dbName+"].dbo.mw_items (id INT IDENTITY PRIMARY KEY, label NVARCHAR(255))")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO ["+dbName+"].dbo.mw_items (label) VALUES ('tenanted-mssql-data')")
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "USE [master]")
		pool.Exec(context.Background(), "DROP DATABASE IF EXISTS ["+dbName+"]")
	})

	tenancy := db.NewMSSQLDatabaseTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	}, "master")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var lbl string
		row, qrErr := pool.QueryRow(r.Context(), "SELECT label FROM mw_items WHERE label = $1", "tenanted-mssql-data")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		if err := row.Scan(&lbl); err != nil {
			t.Logf("tenant-scoped query: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if lbl != "tenanted-mssql-data" {
			t.Errorf("label = %q, want 'tenanted-mssql-data'", lbl)
		}
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{
		TenantID: slug,
		Roles:    []string{"admin"},
	}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		body := w.Body.String()
		t.Errorf("status = %d, want 200; body: %s", w.Code, body)
	}
}

// TenancyConn: no-tenant pass-through

// TestTenancyConn_NoTenant_Passthrough verifies that when no tenant is in
// context (no JWT claims -> TenantHeader no-ops -> no tenant ID -> TenancyConn
// no-ops), the middleware passes through without acquiring a connection.
func TestTenancyConn_NoTenant_Passthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	var called bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// Verify no tenant connection is on ctx.
		if c := db.TenantConn(r.Context()); c != nil {
			t.Error("TenantConn should be nil when no tenant is in context")
		}
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// No JWT claims on context: TenantHeader sees no tenant.
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TenancyConn: nil arguments -> no-op

// TestTenancyConn_NilDatabase_NoOpIntegration verifies nil database + nil
// tenancy cause the middleware to be a no-op (must not panic).
func TestTenancyConn_NilDatabase_NoOpIntegration(t *testing.T) {
	// Full chain: TenantHeader sees tenant -> TenancyConn sees nil db -> no-op -> handler still runs.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(nil, nil)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{
		TenantID: "acme",
		Roles:    []string{"admin"},
	}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TenancyConn: JWT has nil TenantID -> pass-through

// TestTenancyConn_JWTNoTenantID_Passthrough verifies that when JWT claims
// have no TenantID field, the middleware passes through.
func TestTenancyConn_JWTNoTenantID_Passthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	var called bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// Claims exist but TenantID is empty. The role is super_admin because
	// TenantHeader refuses any other role that resolves to no tenant on a
	// multi-tenant install. The passthrough under test is what the cross-tenant
	// scope gets.
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{
		Roles: []string{"super_admin"},
		// TenantID intentionally empty
	}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}
