package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// The archived-tenant guard lets the tenant lifecycle routes through so a
// super admin can restore an archived tenant. A tenant's own admin gains
// nothing from that exemption: a plugin route under the same prefix that the
// tenant's admin may call is still a write to an archived tenant, and it is
// refused with 423 like any other.
func TestAdminRouter_ArchivedTenantLifecycleExemptionIsForSuperAdmins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	users := db.NewUserStore(pool)
	ctx := context.Background()
	const hash = "$2a$10$placeholderhashforlifecycleXXXXXXXXXXXXXXXXXXXXXXXXX"
	admin, err := users.Create(ctx, "archived-admin-"+uuid.NewString()[:8]+"@example.com", hash, []string{"admin"}, "default")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	super, err := users.Create(ctx, "archived-super-"+uuid.NewString()[:8]+"@example.com", hash, []string{"super_admin"}, "default")
	if err != nil {
		t.Fatalf("create super admin: %v", err)
	}

	cfg := testConfig()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	archived := func(context.Context, string) (bool, bool) { return true, true }
	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)),
		WithArchivedChecker(archived),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name: "test-plugin",
			Routes: []plugin.RouteDecl{
				{Method: http.MethodPost, Pattern: "/api/admin/tenants/{id}/replication", Group: plugin.GroupAdmin, Handler: ok},
				{Method: http.MethodPost, Pattern: "/api/admin/tenants/{id}/restore", Group: plugin.GroupSuperAdmin, Handler: ok},
			},
		}}))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	post := func(roles []string, id uuid.UUID, email string, version int, path string) int {
		t.Helper()
		tok, err := auth.Sign(cfg.JWTSecret, 3600, id, email, roles, "default", version, time.Time{})
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := post(admin.Roles, admin.ID, admin.Email, admin.TokenVersion, "/api/admin/tenants/default/replication"); got != http.StatusLocked {
		t.Errorf("an archived tenant's admin writing under the tenants prefix = %d, want 423", got)
	}
	if got := post(super.Roles, super.ID, super.Email, super.TokenVersion, "/api/admin/tenants/default/restore"); got != http.StatusOK {
		t.Errorf("a super admin restoring an archived tenant = %d, want 200", got)
	}
}
