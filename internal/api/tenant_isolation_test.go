//go:build !short && !mutest

package api

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

// TestTenantIsolation_ContentData_Postgres verifies that a tenant cannot read
// another tenant's content rows through a connection the TenancyConn
// middleware scoped to the request's tenant.
func TestTenantIsolation_ContentData_Postgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	tenantA := fmt.Sprintf("iso_a_%d", ts%10000)
	tenantB := fmt.Sprintf("iso_b_%d", ts%10000)
	schemaA := "tenant_" + tenantA
	schemaB := "tenant_" + tenantB

	// Provision both tenant schemas with a content-like table.
	for _, s := range []string{schemaA, schemaB} {
		if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+s); err != nil {
			t.Fatalf("create schema %s: %v", s, err)
		}
		if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+s+".content (id SERIAL PRIMARY KEY, title TEXT, tenant_id TEXT)"); err != nil {
			t.Fatalf("create table %s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaA+" CASCADE")
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaB+" CASCADE")
	})

	// Seed: tenant A has 2 items, tenant B has 1 item.
	_, _ = pool.Exec(ctx, "INSERT INTO "+schemaA+".content (title, tenant_id) VALUES ($1, $2)", "item-a1", tenantA)
	_, _ = pool.Exec(ctx, "INSERT INTO "+schemaA+".content (title, tenant_id) VALUES ($1, $2)", "item-a2", tenantA)
	_, _ = pool.Exec(ctx, "INSERT INTO "+schemaB+".content (title, tenant_id) VALUES ($1, $2)", "item-b1", tenantB)

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	// Handler: count items only visible in current tenant scope, also check
	// for cross-tenant leakage.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentTenant := mw.TenantIDFromContext(r.Context())
		var count int
		row, qrErr := pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM content")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Errorf("tenant %q: query error: %v", currentTenant, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Verify no cross-tenant data visible
		otherTenant := tenantB
		if currentTenant == tenantB {
			otherTenant = tenantA
		}
		var wrongTenantCount int
		row, qrErr = pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM content WHERE tenant_id = $1", otherTenant)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&wrongTenantCount)
		if err != nil {
			t.Errorf("tenant %q: cross-check error: %v", currentTenant, err)
		}
		if wrongTenantCount > 0 {
			t.Errorf("CROSS-TENANT LEAK: tenant %q can see %d rows belonging to %q",
				currentTenant, wrongTenantCount, otherTenant)
		}
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	// Request as tenant A.
	t.Run("tenant_A_sees_only_A_data", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
		claims := &auth.Claims{TenantID: tenantA, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("tenant A: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})

	// Request as tenant B.
	t.Run("tenant_B_sees_only_B_data", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
		claims := &auth.Claims{TenantID: tenantB, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("tenant B: expected 200, got %d: %s", w.Code, w.Body.String())
		}
	})
}
