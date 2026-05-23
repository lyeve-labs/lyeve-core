package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
)

// On a single-tenant install an API key with no tenant claim resolves the
// implicit tenant, as a JWT does. API keys are the machine-to-machine
// credential, so every integration against a default install depends on it.

func tenantForAPIKey(t *testing.T, multiTenant bool, claims *core.AuthClaims, header string, validators ...core.TenantValidatorFunc) (string, int) {
	t.Helper()
	seen := ""
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = mw.TenantIDFromCtx(r)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		req.Header.Set("X-Tenant-ID", header)
	}
	if claims != nil {
		req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, claims))
	}
	rec := httptest.NewRecorder()
	mw.TenantHeader(multiTenant, validators...)(next).ServeHTTP(rec, req)
	return seen, rec.Code
}

func TestTenantHeader_ApiKeyResolvesTheImplicitTenantWhenSingleTenant(t *testing.T) {
	tenant, code := tenantForAPIKey(t, false, &core.AuthClaims{UserID: "k1", IsAPIKey: true, Roles: []string{"admin"}}, "")

	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, mw.ImplicitTenant, tenant,
		"an API key must resolve the same tenant a JWT does on a single-tenant deployment")
}

// A key that names a tenant keeps it, single-tenant or not.
func TestTenantHeader_ApiKeyKeepsItsOwnTenant(t *testing.T) {
	for _, multi := range []bool{false, true} {
		tenant, code := tenantForAPIKey(t, multi, &core.AuthClaims{UserID: "k1", IsAPIKey: true, TenantID: "acme"}, "")
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, "acme", tenant, "multiTenant=%v", multi)
	}
}

// Under multi-tenancy a key with no tenant resolves none and is refused. There
// is no implicit tenant to fall back to, and guessing one would be a
// cross-tenant read waiting to happen. Proceeding without one is no better,
// because the empty tenant is the cross-tenant scope (core.ResolveTenantScope).
func TestTenantHeader_ApiKeyWithNoTenantUnderMultiTenancyIsRefused(t *testing.T) {
	tenant, code := tenantForAPIKey(t, true, &core.AuthClaims{UserID: "k1", IsAPIKey: true, Roles: []string{"admin"}}, "")

	assert.Equal(t, http.StatusForbidden, code)
	assert.Empty(t, tenant)
}

// A super_admin key holds the cross-tenant scope the others are refused.
func TestTenantHeader_ApiKeySuperAdminWithNoTenantKeepsTheGlobalScope(t *testing.T) {
	tenant, code := tenantForAPIKey(t, true, &core.AuthClaims{UserID: "k1", IsAPIKey: true, Roles: []string{"super_admin"}}, "")

	assert.Equal(t, http.StatusOK, code)
	assert.Empty(t, tenant)
}

// The super_admin override still validates a slug that is not where the
// request already is.
func TestTenantHeader_ApiKeySuperAdminOverrideIsStillValidated(t *testing.T) {
	roster := rosterOf("acme")

	tenant, code := tenantForAPIKey(t, true, &core.AuthClaims{UserID: "k1", IsAPIKey: true, Roles: []string{"super_admin"}}, "acme", roster)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "acme", tenant)

	_, code = tenantForAPIKey(t, true, &core.AuthClaims{UserID: "k1", IsAPIKey: true, Roles: []string{"super_admin"}}, "nope", roster)
	assert.Equal(t, http.StatusNotFound, code)
}

// A JWT still wins: it is checked first and the API-key branch only runs when
// nothing has resolved a tenant yet.
func TestTenantHeader_AJwtTenantIsNotOverriddenByAnApiKey(t *testing.T) {
	seen := ""
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = mw.TenantIDFromCtx(r) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, &auth.Claims{UserID: "u1", TenantID: "from-jwt"})
	ctx = context.WithValue(ctx, core.ClaimsKey, &core.AuthClaims{TenantID: "from-key"})
	req = req.WithContext(ctx)

	mw.TenantHeader(true)(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "from-jwt", seen)
}
