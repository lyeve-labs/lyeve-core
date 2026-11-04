package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// The debug routes report on the whole process, so a tenant admin reads none
// of them, and a write to one from a cookie session carries the CSRF token
// like every other write on the admin API.
func TestAdminRouter_DebugRoutesSitInTheAuthenticatedGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	users := db.NewUserStore(pool)
	cfg := testConfig()
	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	token := func(roles ...string) string {
		t.Helper()
		user, err := users.Create(context.Background(), "debug-"+uuid.NewString()[:8]+"@example.com",
			"$2a$10$placeholderhashfordebugroutesXXXXXXXXXXXXXXXXXXXXXXX", roles, "default")
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		tok, err := auth.Sign(cfg.JWTSecret, 3600, user.ID, user.Email, user.Roles, "default", user.TokenVersion)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return tok
	}
	admin, super := token("admin"), token("super_admin")

	get := func(path, tok string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, path := range []string{"/api/admin/debug/latency", "/api/admin/debug/goroutines"} {
		if got := get(path, admin); got != http.StatusForbidden {
			t.Errorf("%s as a tenant admin = %d, want 403", path, got)
		}
		if got := get(path, super); got != http.StatusOK {
			t.Errorf("%s as a super admin = %d, want 200", path, got)
		}
	}

	// The body is not JSON, so a request that reached the handler would be
	// answered 400 and change nothing.
	req := httptest.NewRequest(http.MethodPost, "/api/admin/debug/gc-config", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: security.SessionCookieNameFor(cfg.SecureCookie), Value: super})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "csrf") {
		t.Errorf("POST /debug/gc-config from a cookie session without a CSRF token = %d %s, want a 403 CSRF refusal", rec.Code, rec.Body.String())
	}
}
