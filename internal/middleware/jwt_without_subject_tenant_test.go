package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/stretchr/testify/assert"
)

// On a single-tenant install a JWT with no tenant claim runs as the implicit
// tenant whether or not it has a subject. A credentialed request that still
// resolves no tenant is refused on any install, because the empty tenant is
// the cross-tenant scope (core.ResolveTenantScope) and has to be held, never
// fallen into.

func tenantForJWT(t *testing.T, multiTenant bool, claims *auth.Claims) (string, int) {
	t.Helper()
	seen := ""
	reached := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		seen = mw.TenantIDFromCtx(r)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if claims != nil {
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	}
	rec := httptest.NewRecorder()
	mw.TenantHeader(multiTenant)(next).ServeHTTP(rec, req)
	if !reached {
		return "", rec.Code
	}
	return seen, rec.Code
}

func TestTenantHeader_JwtWithoutSubjectResolvesTheImplicitTenant(t *testing.T) {
	tenant, code := tenantForJWT(t, false, &auth.Claims{Roles: []string{"admin"}})

	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, mw.ImplicitTenant, tenant,
		"a single-tenant install has one answer, so a token with no subject must resolve it rather than the empty scope")
}

func TestTenantHeader_JwtWithoutSubjectNeverReachesTheHandlerUnscoped(t *testing.T) {
	for _, multi := range []bool{false, true} {
		tenant, code := tenantForJWT(t, multi, &auth.Claims{Roles: []string{"admin"}})

		if code == http.StatusOK {
			assert.NotEmpty(t, tenant,
				"multiTenant=%v: a credentialed request reached the handler with no tenant scope", multi)
			continue
		}
		assert.Equal(t, http.StatusForbidden, code, "multiTenant=%v", multi)
	}
}

// The empty scope stays available to the role that is supposed to have it.
func TestTenantHeader_SuperAdminKeepsTheGlobalScopeUnderMultiTenancy(t *testing.T) {
	tenant, code := tenantForJWT(t, true, &auth.Claims{UserID: "u1", Roles: []string{"super_admin"}})

	assert.Equal(t, http.StatusOK, code)
	assert.Empty(t, tenant, "super_admin reads across tenants, so the empty scope is the correct answer")
}

// A request carrying no credential at all is not what this guard is about, and
// the public surface depends on it still being served.
func TestTenantHeader_AnonymousRequestIsNotRefused(t *testing.T) {
	tenant, code := tenantForJWT(t, false, nil)

	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, mw.ImplicitTenant, tenant,
		"a public route on a single-tenant install resolves the one tenant the admin API writes to")
}
