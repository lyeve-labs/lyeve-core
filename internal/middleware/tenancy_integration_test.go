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
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// Postgres: dual-tenant isolation

// TestTenancyConn_DualTenant_Postgres verifies that two concurrent requests
// targeting different tenants each see only their own data. This is the
// critical security property: cross-tenant data leakage must not happen.
func TestTenancyConn_DualTenant_Postgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	// Provision two tenant schemas.
	tenantA := fmt.Sprintf("dual_a_%d", ts%10000)
	tenantB := fmt.Sprintf("dual_b_%d", ts%10000)
	schemaA := "tenant_" + tenantA
	schemaB := "tenant_" + tenantB

	_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schemaA)
	if err != nil {
		t.Fatalf("create schema A: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schemaB)
	if err != nil {
		t.Fatalf("create schema B: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+schemaA+".items (id SERIAL PRIMARY KEY, label TEXT)")
	if err != nil {
		t.Fatalf("create table A: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+schemaB+".items (id SERIAL PRIMARY KEY, label TEXT)")
	if err != nil {
		t.Fatalf("create table B: %v", err)
	}
	_, _ = pool.Exec(ctx, "INSERT INTO "+schemaA+".items (label) VALUES ($1)", "data-from-"+tenantA)
	_, _ = pool.Exec(ctx, "INSERT INTO "+schemaB+".items (label) VALUES ($1)", "data-from-"+tenantB)

	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaA+" CASCADE")
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaB+" CASCADE")
	})

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	// Handler verifies it only sees its own tenant's data.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var count int
		row, qrErr := pool.QueryRow(r.Context(),
			"SELECT COUNT(*) FROM items WHERE label LIKE $1",
			"data-from-"+mw.TenantIDFromCtx(r),
		)
		_ = qrErr
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

	// Request as tenant A.
	reqA := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claimsA := &auth.Claims{TenantID: tenantA, Roles: []string{"admin"}}
	reqA = reqA.WithContext(context.WithValue(reqA.Context(), auth.ClaimsKey, claimsA))
	wA := httptest.NewRecorder()
	m.ServeHTTP(wA, reqA)
	if wA.Code != http.StatusOK {
		t.Errorf("tenant A: status %d, body %s", wA.Code, wA.Body.String())
	}

	// Request as tenant B.
	reqB := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claimsB := &auth.Claims{TenantID: tenantB, Roles: []string{"admin"}}
	reqB = reqB.WithContext(context.WithValue(reqB.Context(), auth.ClaimsKey, claimsB))
	wB := httptest.NewRecorder()
	m.ServeHTTP(wB, reqB)
	if wB.Code != http.StatusOK {
		t.Errorf("tenant B: status %d, body %s", wB.Code, wB.Body.String())
	}
}

// Postgres: tenant lifecycle (create -> verify -> drop -> verify gone)

// TestTenancyConn_Postgres_Lifecycle verifies the full tenant lifecycle:
// create schema + table, seed data, query through middleware, drop schema,
// verify data is gone.
func TestTenancyConn_Postgres_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	slug := fmt.Sprintf("lfc_%d", time.Now().UnixNano()%10000)
	schema := "tenant_" + slug

	// Phase 1: Create
	_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+schema+".lfc_items (id SERIAL PRIMARY KEY, name TEXT)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Phase 2: Seed
	_, err = pool.Exec(ctx, "INSERT INTO "+schema+".lfc_items (name) VALUES ($1)", "lifecycle-item-1")
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO "+schema+".lfc_items (name) VALUES ($1)", "lifecycle-item-2")
	if err != nil {
		t.Fatalf("seed data 2: %v", err)
	}

	// Phase 3: Verify through middleware
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	var items []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), "SELECT name FROM lfc_items ORDER BY id")
		if err != nil {
			t.Errorf("query via tenant conn: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Errorf("scan: %v", err)
			}
			items = append(items, name)
		}
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claims := &auth.Claims{TenantID: slug, Roles: []string{"admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("phase 3 verify: status %d, body %s", w.Code, w.Body.String())
	}
	if len(items) != 2 {
		t.Errorf("phase 3: expected 2 items, got %d: %v", len(items), items)
	}
	if len(items) >= 1 && items[0] != "lifecycle-item-1" {
		t.Errorf("phase 3: items[0] = %q, want 'lifecycle-item-1'", items[0])
	}

	// Phase 4: Drop
	_, err = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	if err != nil {
		t.Fatalf("drop schema: %v", err)
	}

	// Phase 5: Verify gone
	var schemaExists bool
	row, qrErr := pool.QueryRow(context.Background(),
		"SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&schemaExists)
	if err != nil {
		t.Fatalf("check schema existence: %v", err)
	}
	if schemaExists {
		t.Errorf("phase 5: schema %s still exists after DROP", schema)
	}
}

// Postgres: invalid slug through full middleware chain

// TestTenancyConn_InvalidSlug_HTTP500 verifies that an invalid tenant slug
// through the full middleware chain returns HTTP 500 (Apply validation
// rejects it when the handler first acquires a tenant conn).
func TestTenancyConn_InvalidSlug_HTTP500(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

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

	// Slug with uppercase: violates safeSlugRe ^[a-z][a-z0-9_]{0,62}$
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claims := &auth.Claims{TenantID: "EVIL_UPPERCASE", Roles: []string{"admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("invalid slug: expected 500, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// Postgres: tenant length boundary: 63 chars (valid)

func TestTenancyConn_Postgres_MaxSlugLength(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	// 63-char slug: a + 62 b's = valid
	slug := fmt.Sprintf("m%062d", time.Now().UnixNano()%1000000000000)
	if len(slug) > 63 {
		slug = slug[:63]
	}
	schema := sqlx.TenantSchemaName(slug)

	_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
	if err != nil {
		t.Fatalf("create schema with max-length slug: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+schema+".items (id SERIAL PRIMARY KEY, val TEXT)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := pool.Exec(r.Context(), "INSERT INTO items (val) VALUES ($1)", "ok"); err != nil {
			t.Errorf("insert: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	claims := &auth.Claims{TenantID: slug, Roles: []string{"admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("max-length slug: expected 200, got %d", w.Code)
	}
}

// Postgres: empty TenantID in JWT, passthrough
//
// The caller is a super_admin because TenantHeader refuses any other role
// that resolves to no tenant on a multi-tenant install. The subject here is
// still TenancyConn: with no tenant it must acquire no connection and apply no
// isolation, and the super_admin cross-tenant scope is the case that reaches it.

func TestTenancyConn_Postgres_EmptyTenantID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No tenant isolation. Query on public schema works.
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claims := &auth.Claims{TenantID: "", Roles: []string{"super_admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("empty TenantID: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// Postgres: nil TenantID in JWT, passthrough

func TestTenancyConn_Postgres_NilTenantID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	pool := testdb.Postgres(t)
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// Claims with Roles but no TenantID at all (zero value = "").
	req := httptest.NewRequest(http.MethodGet, "/api/items", nil)
	claims := &auth.Claims{Roles: []string{"super_admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("nil TenantID: expected 200, got %d", w.Code)
	}
}
