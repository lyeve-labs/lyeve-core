package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// probeTenant runs one request through TenantHeader and reports the tenant the
// downstream handler saw, plus the status if the middleware answered itself.
func probeTenant(t *testing.T, multiTenant bool, header string, withClaims bool, validators ...core.TenantValidatorFunc) (string, int) {
	t.Helper()
	seen := ""
	h := TenantHeader(multiTenant, validators...)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = core.TenantIDFromCtx(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	if header != "" {
		req.Header.Set("X-Tenant-ID", header)
	}
	if withClaims {
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
			&auth.Claims{UserID: "u1", TenantID: "", Roles: []string{"editor"}}))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return seen, rec.Code
}

// TestTenantHeader_AnonymousRequestGetsTheImplicitTenant covers the public
// plugin routes on a single-tenant install, which is the default shape.
//
// They carry no credential, so no resolution branch finds a tenant, and their
// stores refuse through core.RequireTenantID without one. There is exactly one
// tenant on such an install, so there is exactly one right answer.
func TestTenantHeader_AnonymousRequestGetsTheImplicitTenant(t *testing.T) {
	seen, code := probeTenant(t, false, "", false)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if seen != ImplicitTenant {
		t.Errorf("tenant = %q, want %q", seen, ImplicitTenant)
	}
}

// TestTenantHeader_AnonymousHeaderIsIgnoredUnderMultiTenancy pins what the
// implicit tenant must never extend to.
//
// Resolving an anonymous X-Tenant-ID means checking the slug against the
// roster, and answering differently for a tenant that exists than for one that
// does not makes every public route a tenant enumeration oracle. It would also
// break first-run setup, which has to answer before any tenant exists. Domain
// routing is the supported way to name a tenant for an anonymous caller.
func TestTenantHeader_AnonymousHeaderIsIgnoredUnderMultiTenancy(t *testing.T) {
	consulted := false
	valid := func(_ context.Context, _ string) bool {
		consulted = true
		return true
	}
	seen, code := probeTenant(t, true, "brand_b", false, valid)
	if consulted {
		t.Error("the roster was consulted for a request that carries no credential")
	}
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200: the header is ignored, not refused", code)
	}
	if seen != "" {
		t.Errorf("tenant = %q, want empty: an anonymous header must not resolve", seen)
	}
}

// TestTenantHeader_AuthenticatedWithNoTenantIsStillRefused guards the boundary
// the anonymous path must not erode. An account that carries a credential but
// no tenant is a misconfiguration, and letting it fall through to the implicit
// tenant would hand it a scope it was never granted.
func TestTenantHeader_AuthenticatedWithNoTenantIsStillRefused(t *testing.T) {
	seen, code := probeTenant(t, true, "", true)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a credential that names no tenant", code)
	}
	if seen != "" {
		t.Errorf("tenant = %q, want empty", seen)
	}
}
