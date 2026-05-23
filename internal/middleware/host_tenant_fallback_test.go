package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// tenantOnHost runs one request through TenantHeader on a multi-tenant install
// after domain routing put hostTenant on the context, the way the host
// resolver does for a mapped domain.
func tenantOnHost(t *testing.T, hostTenant string, claims *auth.Claims) (string, int) {
	t.Helper()
	seen, reached := "", false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		seen = mw.TenantIDFromCtx(r)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := core.WithTenantID(req.Context(), hostTenant)
	if claims != nil {
		ctx = context.WithValue(ctx, auth.ClaimsKey, claims)
	}
	rec := httptest.NewRecorder()
	mw.TenantHeader(true)(next).ServeHTTP(rec, req.WithContext(ctx))
	if !reached {
		return "", rec.Code
	}
	return seen, rec.Code
}

// A mapped domain names the tenant of a request that carries no credential.
func TestTenantHeader_HostTenantScopesAnAnonymousRequest(t *testing.T) {
	tenant, code := tenantOnHost(t, "acme", nil)

	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "acme", tenant)
}

// A session's own tenant wins over the domain it was used on.
func TestTenantHeader_ClaimTenantWinsOverTheHost(t *testing.T) {
	tenant, code := tenantOnHost(t, "acme", &auth.Claims{UserID: "u1", TenantID: "globex", Roles: []string{"admin"}})

	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "globex", tenant)
}

// An account whose claim names no tenant is refused on a mapped domain as
// everywhere else.
func TestTenantHeader_EmptyClaimIsRefusedOnAMappedDomain(t *testing.T) {
	_, code := tenantOnHost(t, "acme", &auth.Claims{UserID: "u1", Roles: []string{"admin"}})

	assert.Equal(t, http.StatusForbidden, code)
}

// A super admin on a mapped domain works in that domain's tenant, and both
// tenant keys agree, so one request has one scope.
func TestTenantHeader_SuperAdminTakesTheHostTenantOnBothKeys(t *testing.T) {
	seen, coreSeen, reached := "", "", false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		seen = mw.TenantIDFromCtx(r)
		coreSeen = core.TenantIDFromCtx(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := core.WithTenantID(req.Context(), "acme")
	ctx = context.WithValue(ctx, auth.ClaimsKey, &auth.Claims{UserID: "u1", Roles: []string{"super_admin"}})
	mw.TenantHeader(true)(next).ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))

	assert.True(t, reached)
	assert.Equal(t, "acme", seen)
	assert.Equal(t, coreSeen, seen)
}

// The archived-tenant guard reads TenantHeader's key, so a super admin's write
// on an archived tenant's domain is refused.
func TestTenantHeader_ArchivedHostTenantStaysReadOnlyForASuperAdmin(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached = true })
	archived := func(_ context.Context, slug string) (bool, bool) { return slug == "acme", true }
	h := mw.TenantHeader(true)(mw.ReadOnlyArchived(archived)(next))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := core.WithTenantID(req.Context(), "acme")
	ctx = context.WithValue(ctx, auth.ClaimsKey, &auth.Claims{UserID: "u1", Roles: []string{"super_admin"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))

	assert.False(t, reached)
	assert.Equal(t, http.StatusLocked, rec.Code)
}

// An API key that names no tenant is refused on a mapped domain like a session.
func TestTenantHeader_ApiKeyWithNoTenantIsRefusedOnAMappedDomain(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { reached = true })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := core.WithTenantID(req.Context(), "acme")
	ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{UserID: "k1", Roles: []string{"admin"}})
	rec := httptest.NewRecorder()
	mw.TenantHeader(true)(next).ServeHTTP(rec, req.WithContext(ctx))

	assert.False(t, reached)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
