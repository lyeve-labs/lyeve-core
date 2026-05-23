package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// A plugin route must refuse an access token whose user has since logged out,
// been disabled or had a password set, as the engine's own routes do.
func TestPluginRoutes_RefuseARevokedSession(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/auth", Group: plugin.GroupAuth, Handler: ok},
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/admin", Group: plugin.GroupAdmin, Handler: ok},
			{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/super", Group: plugin.GroupSuperAdmin, Handler: ok},
		},
	}}

	const currentVersion = 3
	check := tokenVersionCheck(func(context.Context, string) (int, error) { return currentVersion, nil })
	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", false, 1<<20, nil, nil, check)

	for _, path := range []string{"/test-plugin/auth", "/test-plugin/admin", "/test-plugin/super"} {
		for _, tc := range []struct {
			name    string
			version int
			want    int
		}{
			{"revoked", currentVersion - 1, http.StatusUnauthorized},
			{"current", currentVersion, http.StatusOK},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				claims := makeClaims(uuid.New(), "owner@test.com", []string{"super_admin"})
				claims.TokenVersion = tc.version
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d", rec.Code, tc.want)
				}
			})
		}
	}
}

// The same, through the router the engine serves: a plugin's admin route and
// the debug routes, with the version read from the database.
func TestAdminRouter_RevokedSessionEndsOnPluginAndDebugRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.Postgres(t)
	users := db.NewUserStore(pool)
	user, err := users.Create(context.Background(), "revoked-"+uuid.NewString()[:8]+"@example.com",
		"$2a$10$placeholderhashforrevocationXXXXXXXXXXXXXXXXXXXXXXXX", []string{"super_admin"}, "default")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	cfg := testConfig()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name:   "test-plugin",
			Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/items", Group: plugin.GroupAdmin, Handler: ok}},
		}}))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	tok, err := auth.Sign(cfg.JWTSecret, 3600, user.ID, user.Email, user.Roles, "default", user.TokenVersion)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	paths := []string{"/api/admin/test-plugin/items", "/api/admin/debug/gc-config"}
	for _, p := range paths {
		if got := get(p); got != http.StatusOK {
			t.Fatalf("%s with a live session = %d, want 200", p, got)
		}
	}

	if _, err := users.SetPassword(context.Background(), user.ID, "$2a$10$anotherplaceholderhashXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	for _, p := range paths {
		if got := get(p); got != http.StatusUnauthorized {
			t.Errorf("%s after the password was set = %d, want 401", p, got)
		}
	}
}
