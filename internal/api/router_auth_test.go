package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// /api/admin/health and /api/admin/ready are registered inside the
// requireAuth group: unauthenticated requests return 401. They must NOT be
// registered outside the authenticated subgroup.
func TestAdminRouter_HealthAndReadyRequireAuth(t *testing.T) {
	t.Parallel()

	pool := &fakeDB{engine: "postgres"}
	cfg := testConfig()

	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	for _, path := range []string{"/api/admin/health", "/api/admin/ready"} {
		t.Run(strings.TrimPrefix(path, "/api/admin/"), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s: status = %d, want %d (401 Unauthorized); body=%q",
					path, w.Code, http.StatusUnauthorized, w.Body.String())
			}
		})
	}
}

// /api/v1/health and /api/v1/ready are registered inside the requireAuth
// group and return 401 for unauthenticated requests, on the API router.
func TestAPIRouter_HealthAndReadyRequireAuth(t *testing.T) {
	t.Parallel()

	pool := &fakeDB{engine: "postgres"}
	cfg := testConfig()

	router, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	for _, path := range []string{"/api/v1/health", "/api/v1/ready"} {
		t.Run(strings.TrimPrefix(path, "/api/v1/"), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s: status = %d, want %d (401 Unauthorized); body=%q",
					path, w.Code, http.StatusUnauthorized, w.Body.String())
			}
		})
	}
}

// The probes answer for any authenticated caller, including an API key minted
// for unrelated work. They sit outside the scope-enforcing group, where
// RequireScoped would derive "health:read" from the path and refuse every key
// not issued with that exact scope.
func TestAPIRouter_HealthAndReadyIgnoreAPIKeyScopes(t *testing.T) {
	t.Parallel()

	pool := &fakeDB{engine: "postgres"}
	cfg := testConfig()

	injectKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := &core.AuthClaims{
				UserID:   uuid.NewString(),
				Roles:    []string{"viewer"},
				Scopes:   []string{"content:read"},
				IsAPIKey: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}

	router, err := NewAPIRouter(pool, cfg, nil, WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectKey))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	for _, path := range []string{"/api/v1/health", "/api/v1/ready"} {
		t.Run(strings.TrimPrefix(path, "/api/v1/"), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("GET %s with a content-scoped key: status = %d, want 200; body=%q",
					path, w.Code, w.Body.String())
			}
		})
	}

	// A scoped route in the same realm still enforces the scope, so the probes
	// are an exception and not a hole in RequireScoped.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/schemas", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("GET /api/v1/schemas with a content-scoped key: status = %d, want 403", w.Code)
	}
}

// The admin router applies the API key auth middleware the runtime wires
// through WithAPIKeyAuth, as the API router does, so a scoped key
// authenticates on an admin route rather than falling through to a bare 401.
func TestAdminRouter_APIKeyAuthApplied(t *testing.T) {
	t.Parallel()

	pool := &fakeDB{engine: "postgres"}
	cfg := testConfig()

	injectKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := &core.AuthClaims{
				UserID:   uuid.NewString(),
				Roles:    []string{"viewer"},
				Scopes:   []string{"content:read"},
				IsAPIKey: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}

	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectKey))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	// Probes accept any authenticated caller: the key must be recognized.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET /api/admin/health with a content-scoped key: status = %d, want 200; body=%q", w.Code, w.Body.String())
	}

	// admin/super_admin route: the key is authenticated but lacks the role, so
	// the response is 403 "insufficient scope", never 401.
	req = httptest.NewRequest(http.MethodPost, "/api/admin/users", nil)
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("POST /api/admin/users with a content-scoped key: status = %d, want 403; body=%q", w.Code, w.Body.String())
	}
}
