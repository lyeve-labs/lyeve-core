package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Plugin middleware supplied through WithMiddleware runs on the admin router
// and must see the tenant the request resolved to. Quota enforcement, WAF and
// the per-tenant limiter all read it with core.TenantIDFromCtx and take a
// no-tenant branch when it is empty, so mounting them above the tenancy
// middleware disables them silently rather than loudly.
func TestAdminRouter_ExtraMiddlewareSeesResolvedTenant(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.MultiTenant = true

	var seen string
	var ran bool
	probe := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ran = true
			seen = core.TenantIDFromCtx(r.Context())
			next.ServeHTTP(w, r)
		})
	}

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)), WithMiddleware(probe))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "ops@test.com", []string{"super_admin"}, "", 1)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tenant-ID", "acme")
	router.ServeHTTP(httptest.NewRecorder(), req)

	if !ran {
		t.Fatal("extra middleware never ran on /api/admin/schemas")
	}
	if seen != "acme" {
		t.Errorf("extra middleware saw tenant %q, want %q: tenancy resolves below it in the chain", seen, "acme")
	}
}

// The tenant a JWT claims must reach the same middleware without a header.
func TestAdminRouter_ExtraMiddlewareSeesClaimTenant(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.MultiTenant = true

	var seen string
	probe := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = core.TenantIDFromCtx(r.Context())
			next.ServeHTTP(w, r)
		})
	}

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)), WithMiddleware(probe))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	token, err := auth.Sign(cfg.JWTSecret, 3600, uuid.New(), "editor@test.com", []string{"editor"}, "beta", 1)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "beta" {
		t.Errorf("extra middleware saw tenant %q, want %q", seen, "beta")
	}
}

// A credential-less caller resolves no tenant, so the validator that guards
// super_admin header overrides must not be consulted and must not reject.
// Tenancy sits on the outer chain, where unauthenticated paths also live.
func TestAdminRouter_UnauthenticatedRequestSkipsTenantValidator(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.MultiTenant = true

	consulted := false
	validator := func(_ context.Context, _ string) bool {
		consulted = true
		return false
	}

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)), WithTenantValidator(validator))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil)
	req.Header.Set("X-Tenant-ID", "nope")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if consulted {
		t.Error("tenant validator was consulted for a request that carries no claims")
	}
	if w.Code == http.StatusNotFound {
		t.Errorf("GET /api/admin/setup answered 404: an unauthenticated X-Tenant-ID must not be validated")
	}
}
