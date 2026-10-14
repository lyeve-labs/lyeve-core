package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// nonCanonicalTenants are tenant names a token can carry that the engine
// never issues.
var nonCanonicalTenants = []string{"Acme", "acme-corp", "1acme", "acme;drop", " acme", "tenant_" + string(make([]byte, 63))}

// serveTokenTenant runs TenantHeader for a token naming tenant, with a super
// admin's header naming another, and reports the response and the tenant the
// handler saw, if it ran.
func serveTokenTenant(multiTenant bool, tenant string, roles []string) (*httptest.ResponseRecorder, string, bool) {
	var seen string
	ran := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ran = true
		seen = mw.TenantIDFromCtx(r)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "acme")
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		&auth.Claims{UserID: "u1", TenantID: tenant, Roles: roles}))
	rec := httptest.NewRecorder()
	mw.TenantHeader(multiTenant)(next).ServeHTTP(rec, req)
	return rec, seen, ran
}

// On a multi-tenant install a session token whose tenant is not a slug the
// engine issues is refused before any handler runs, whatever header a super
// admin sends. Scoping a tenant connection refused it once a request read,
// but a stream that never reads ran under the name the token gave.
func TestTenantHeader_RefusesATokenTenantThatIsNotCanonical(t *testing.T) {
	for _, tenant := range nonCanonicalTenants {
		for _, roles := range [][]string{{"editor"}, {"super_admin"}} {
			rec, _, ran := serveTokenTenant(true, tenant, roles)

			assert.Equal(t, http.StatusUnauthorized, rec.Code, "tenant=%q roles=%v", tenant, roles)
			assert.JSONEq(t, `{"error":"invalid token tenant"}`, rec.Body.String())
			assert.False(t, ran, "the handler ran for tenant %q", tenant)
		}
	}
}

// A single-tenant install scopes no tenant connection, so its accounts keep
// the tenant they carry whatever its spelling, and a canonical tenant passes
// on either install.
func TestTenantHeader_AdmitsATokenTenantWhereNoConnectionIsScoped(t *testing.T) {
	for _, tenant := range nonCanonicalTenants {
		rec, seen, ran := serveTokenTenant(false, tenant, []string{"editor"})

		assert.Equal(t, http.StatusOK, rec.Code, "tenant=%q", tenant)
		assert.True(t, ran, "the handler did not run for tenant %q", tenant)
		assert.Equal(t, tenant, seen)
	}
	for _, tc := range []struct {
		multiTenant bool
		claim, want string
	}{
		{true, "acme_corp", "acme_corp"},
		{false, "acme_corp", "acme_corp"},
		{false, "", mw.ImplicitTenant},
	} {
		rec, seen, ran := serveTokenTenant(tc.multiTenant, tc.claim, []string{"editor"})

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.True(t, ran)
		assert.Equal(t, tc.want, seen)
	}
}
