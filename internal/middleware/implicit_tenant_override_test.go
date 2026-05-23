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
	"github.com/stretchr/testify/require"
)

// A single-tenant deployment runs as a tenant nothing ever registers, so the
// roster validator does not know the name the engine gave the request.
//
// The override is compared against the tenant the request already resolved
// to, not against the JWT claim, which for a super_admin with no home tenant
// is empty. Naming the tenant the request already resolved to is not an
// override, and cannot be a slug that does not exist, so it skips the roster.

// rosterOf accepts exactly the slugs given, as sys_tenants does.
func rosterOf(slugs ...string) core.TenantValidatorFunc {
	known := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		known[s] = true
	}
	return func(_ context.Context, slug string) bool { return known[slug] }
}

// resolveTenant runs TenantHeader over a request carrying the given claims and
// header, and reports the tenant the handler saw plus the status written.
func resolveTenant(t *testing.T, multiTenant bool, claims *auth.Claims, header string, validators ...core.TenantValidatorFunc) (string, int) {
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
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	}

	rec := httptest.NewRecorder()
	mw.TenantHeader(multiTenant, validators...)(next).ServeHTTP(rec, req)
	return seen, rec.Code
}

func superAdmin(tenant string) *auth.Claims {
	return &auth.Claims{UserID: "u1", Roles: []string{"super_admin"}, TenantID: tenant}
}

func TestTenantHeader_NamingTheImplicitTenantIsNotAnOverride(t *testing.T) {
	roster := rosterOf("acme") // as in every real deployment, "default" is absent

	tenant, code := resolveTenant(t, false, superAdmin(""), mw.ImplicitTenant, roster)

	assert.Equal(t, http.StatusOK, code,
		"the tenant the request already resolved to cannot be one that does not exist")
	assert.Equal(t, mw.ImplicitTenant, tenant)
}

// The same call with the header left off, so the two paths can be compared.
func TestTenantHeader_ImplicitTenantResolvesWithoutTheHeader(t *testing.T) {
	tenant, code := resolveTenant(t, false, superAdmin(""), "", rosterOf("acme"))

	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, mw.ImplicitTenant, tenant,
		"an authenticated caller naming no tenant runs as the implicit one")
}

// The relaxation is exactly one slug wide: anything the request did not already
// resolve to is still checked against the roster.
func TestTenantHeader_UnknownSlugIsStillRefused(t *testing.T) {
	_, code := resolveTenant(t, false, superAdmin(""), "no_such_tenant", rosterOf("acme"))

	assert.Equal(t, http.StatusNotFound, code,
		"a slug naming no tenant must not resolve just because the caller is super_admin")
}

// A super_admin with a home tenant reaching for another one is a genuine
// override and goes to the roster, whether the target is known or not.
func TestTenantHeader_RealOverrideStillGoesToTheRoster(t *testing.T) {
	roster := rosterOf("acme", "globex")

	tenant, code := resolveTenant(t, true, superAdmin("acme"), "globex", roster)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "globex", tenant)

	_, code = resolveTenant(t, true, superAdmin("acme"), "initech", roster)
	assert.Equal(t, http.StatusNotFound, code,
		"an unregistered target is refused even for a super_admin with a home tenant")
}

// Multi-tenant has no implicit tenant to name, so "default" there is an
// ordinary slug and is refused like any other the roster does not hold.
func TestTenantHeader_ImplicitTenantIsNotSpecialUnderMultiTenancy(t *testing.T) {
	_, code := resolveTenant(t, true, superAdmin("acme"), mw.ImplicitTenant, rosterOf("acme"))

	assert.Equal(t, http.StatusNotFound, code)
}
