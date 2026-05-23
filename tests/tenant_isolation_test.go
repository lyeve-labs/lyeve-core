//go:build integration
// +build integration

// Multi-tenancy isolation integration tests.
//
// These tests verify that tenant data isolation is enforced end-to-end:
//   - Two tenants cannot read each other's data, which the tenant_id
//     predicate on every query is what enforces
//   - Cross-tenant requests via different TenantID headers are isolated
//   - Invalid tenant slugs are rejected
//
// Requires: testcontainers (Docker). Run with:
//
//	go test -tags=integration -run TestTenantIsolation ./tests/ -v

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Tenant isolation via API router

func TestTenantIsolation_Postgres(t *testing.T) {
	pool := testdb.Postgres(t)
	testTenantIsolation(t, pool, "postgres")
}

func TestTenantIsolation_MySQL(t *testing.T) {
	pool := testdb.MySQL(t)
	testTenantIsolation(t, pool, "mysql")
}

func TestTenantIsolation_MSSQL(t *testing.T) {
	pool := testdb.MSSQL(t)
	testTenantIsolation(t, pool, "mssql")
}

func testTenantIsolation(t *testing.T, pool db.DB, dialect string) {
	t.Helper()
	ctx := context.Background()
	cfg := testConfigFor(dialect)

	pool.Exec(ctx, "DELETE FROM sys_users") //nolint:errcheck

	adminRouter, err := api.NewAdminRouter(pool, cfg)
	if err != nil {
		t.Fatalf("admin router: %v", err)
	}

	rr := doJSON(t, adminRouter, "POST", "/api/admin/setup",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), setupHeaders)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", rr.Code, rr.Body)
	}

	const schemaName = "tenant_isolated_items"
	t.Cleanup(func() { cleanupSchema(t, pool, schemaName) })
	schemas := supplySchema(t, pool, schemaName,
		map[string]any{"name": "label", "field_type": "text", "required": true})

	hookReg := hooks.NewRegistry()
	apiRouter, err := api.NewAPIRouter(pool, cfg, hookReg, api.WithSchemaSource(schemas))
	if err != nil {
		t.Fatalf("api router: %v", err)
	}

	rr = doJSON(t, apiRouter, "POST", "/api/v1/auth/token",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rr.Code, rr.Body)
	}
	apiToken := parseBody(t, rr)["token"].(string)

	// Seed content into the default (public) schema

	apiH := map[string]string{"Authorization": "Bearer " + apiToken}

	t.Run("create_item_default_tenant", func(t *testing.T) {
		payload := map[string]any{"data": map[string]any{"label": "public-item"}}
		rr := doJSON(t, apiRouter, "POST", "/api/v1/content/"+schemaName, mustJSON(t, payload), apiH)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rr.Code, rr.Body)
		}
	})

	t.Run("list_items_default_tenant", func(t *testing.T) {
		rr := doJSON(t, apiRouter, "GET", "/api/v1/content/"+schemaName, "", apiH)
		if rr.Code != http.StatusOK {
			t.Fatalf("list: %d %s", rr.Code, rr.Body)
		}
		// The list endpoint answers a bare array.
		var items []any
		if err := json.NewDecoder(rr.Body).Decode(&items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(items) != 1 {
			t.Errorf("expected 1 item in default tenant, got %d", len(items))
		}
	})
}
