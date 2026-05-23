package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// TenantHeader: X-Tenant-ID without JWT claims is ignored

func TestTenantHeader_XTenantID_WithoutJWT(t *testing.T) {
	// X-Tenant-ID without JWT claims must NOT inject a tenant.
	// The header is only a mechanism for super_admin to override. Without
	// claims there's no authenticated user to authorize the override.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "evil_noauth_tenant")
	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "" {
		t.Errorf("X-Tenant-ID without JWT claims: got %q, want empty", tenant)
	}
}

// TenantHeader: super_admin without TenantID in claims

func TestTenantHeader_SuperAdmin_NoJWT_TenantID(t *testing.T) {
	// super_admin role + X-Tenant-ID, but JWT has no TenantID field.
	// The header override should still be honored since the role check
	// happens before the fallback.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "ops_override")
	claims := &auth.Claims{
		Roles: []string{"super_admin"},
		// TenantID intentionally empty: ops use case with no default tenant
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "ops_override" {
		t.Errorf("super_admin no-JWT-tenant + header: got %q, want 'ops_override'", tenant)
	}
}

// TenantHeader: super_admin override with empty TenantID

func TestTenantHeader_SuperAdmin_EmptyTenantID_HeaderFallback(t *testing.T) {
	// super_admin with JWT TenantID="" sets tenant via X-Tenant-ID.
	// The header is checked after the role, so empty JWT TenantID is
	// overwritten.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "from_header")
	claims := &auth.Claims{
		TenantID: "", // empty JWT claim
		Roles:    []string{"super_admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "from_header" {
		t.Errorf("super_admin empty JWT + header: got %q, want 'from_header'", tenant)
	}
}

// TenantHeader: multiTenant=true but no claims, X-Tenant-ID ignored

func TestTenantHeader_XTenantID_Admin_NoTenantInJWT(t *testing.T) {
	// admin role with no TenantID in JWT and X-Tenant-ID header.
	// admin cannot override via header. No JWT TenantID -> no tenant.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "hijack_attempt")
	claims := &auth.Claims{
		Roles: []string{"admin"},
		// TenantID intentionally empty
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "" {
		t.Errorf("admin no-tenant + header: got %q, want empty", tenant)
	}
}

// TenantHeader: response code check

func TestTenantHeader_PreservesStatusCode(t *testing.T) {
	// TenantHeader must not alter the response status set by the handler.
	m := mw.TenantHeader(true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &auth.Claims{
		TenantID: "acme",
		Roles:    []string{"admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if w.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTeapot)
	}
}

// TenantHeader: no-op when multiTenant is false even with claims

func TestTenantHeader_Disabled_WithClaims(t *testing.T) {
	// In single-tenant mode, JWT tenant claims are STILL extracted so
	// downstream stores that call RequireTenantID have a tenant context.
	// This prevents 503 failures on admin CRUD routes.
	m := mw.TenantHeader(false)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &auth.Claims{
		TenantID: "acme",
		Roles:    []string{"admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "acme" {
		t.Errorf("disabled multiTenant with claims: got %q, want %q", tenant, "acme")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TenancyConn: nil database with tenant in context

func TestTenancyConn_NilDatabase_WithTenant(t *testing.T) {
	// Even with a tenant on context, nil database makes TenancyConn a no-op.
	m := mw.TenancyConn(nil, nil)
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	middlewareChain := mw.TenantHeader(true)(m(next))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &auth.Claims{
		TenantID: "acme",
		Roles:    []string{"admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	middlewareChain.ServeHTTP(w, req)

	if !called {
		t.Error("handler was not called")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

// TenantIDFromCtx: via request with nil context-aware body

func TestTenantIDFromCtx_InjectedViaMiddleware(t *testing.T) {
	// Full pipeline: TenantHeader(true) -> handler reads tenant via TenantIDFromCtx.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &auth.Claims{
		TenantID: "myorg",
		Roles:    []string{"editor"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "myorg" {
		t.Errorf("TenantIDFromCtx: got %q, want 'myorg'", tenant)
	}
}

// TenantHeader: X-Tenant-ID non-super_admin ignored, TenantID still honored

func TestTenantHeader_NonSuperAdmin_XTenantID_Ignored_But_TenantID_Honored(t *testing.T) {
	// Non-super_admin with TenantID in JWT + X-Tenant-ID.
	// Header must be ignored. JWT TenantID must be used.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	roles := []string{"admin", "editor", "viewer"}
	for _, role := range roles {
		t.Run("role="+role, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Tenant-ID", "header_override")
			claims := &auth.Claims{
				TenantID: "jwt-tenant",
				Roles:    []string{role},
			}
			ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
			req = req.WithContext(ctx)

			w := httptest.NewRecorder()
			m(next).ServeHTTP(w, req)

			if tenant != "jwt-tenant" {
				t.Errorf("role=%s: got %q, want 'jwt-tenant'", role, tenant)
			}
		})
	}
}

// TenantHeader: no claims at all

func TestTenantHeader_MissingClaimsKey(t *testing.T) {
	// Context has no ClaimsKey set at all. Should be a no-op.
	m := mw.TenantHeader(true)
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := mw.TenantIDFromCtx(r); got != "" {
			t.Errorf("tenant present without claims: %q", got)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No claims injected into context.
	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if !called {
		t.Error("handler was not called")
	}
}

// TenantHeader: claims with wrong type

func TestTenantHeader_WrongClaimsType(t *testing.T) {
	// ClaimsKey has a non-*auth.Claims value. Should be a no-op.
	// The type assertion in TenantHeader uses the ok pattern, so it
	// gracefully handles this.
	m := mw.TenantHeader(true)
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := mw.TenantIDFromCtx(r); got != "" {
			t.Errorf("tenant present with wrong claims type: %q", got)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, "this-is-a-string-not-claims")
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if !called {
		t.Error("handler was not called")
	}
}

// TenantIDFromContext: round-trip through WithValue

func TestTenantIDFromContext_RoundTrip(t *testing.T) {
	// Verify we can inject and retrieve a tenant via the context key.
	ctx := context.Background()

	if got := mw.TenantIDFromContext(ctx); got != "" {
		t.Errorf("empty context: got %q, want empty", got)
	}

	// We can't directly inject since tenantKey is unexported, but we
	// can verify retrieval works through the middleware chain which
	// is tested elsewhere.
}

// TenantHeader: super_admin with X-Tenant-ID empty string

func TestTenantHeader_SuperAdmin_XTenantID_EmptyString_FallsBack(t *testing.T) {
	// When X-Tenant-ID is present but empty string, super_admin falls
	// back to JWT TenantID.
	m := mw.TenantHeader(true)
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "") // empty string header
	claims := &auth.Claims{
		TenantID: "jwt-acme",
		Roles:    []string{"super_admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "jwt-acme" {
		t.Errorf("super_admin empty X-Tenant-ID: got %q, want 'jwt-acme'", tenant)
	}
}

// TenantHeader: super_admin X-Tenant-ID override with multiTenant=false

func TestTenantHeader_SuperAdmin_XTenantID_MultiTenantFalse(t *testing.T) {
	// Even when multiTenant is false, a super_admin with an X-Tenant-ID
	// header should have that tenant injected into context.  The header
	// reflects explicit intent from an authorized caller.
	m := mw.TenantHeader(false) // multiTenant=false
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "ops_override")
	claims := &auth.Claims{
		Roles:    []string{"super_admin"},
		TenantID: "", // super_admin with no default tenant
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant != "ops_override" {
		t.Errorf("super_admin X-Tenant-ID with multiTenant=false: got %q, want 'ops_override'", tenant)
	}
}

func TestTenantHeader_Admin_XTenantID_MultiTenantFalse_Ignored(t *testing.T) {
	// The header is a super_admin mechanism, so an admin's header is ignored
	// and the request runs as the implicit tenant. A single-tenant install has
	// exactly one answer, and a token with no subject, which is what this
	// claim set is, gets it too. It never runs unscoped, because the empty
	// tenant is the cross-tenant scope (core.ResolveTenantScope).
	m := mw.TenantHeader(false) // multiTenant=false
	var tenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "hijack_attempt")
	claims := &auth.Claims{
		Roles:    []string{"admin"},
		TenantID: "", // no default tenant
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if tenant == "hijack_attempt" {
		t.Error("admin X-Tenant-ID with multiTenant=false: the header was honored")
	}
	if tenant != mw.ImplicitTenant {
		t.Errorf("admin X-Tenant-ID with multiTenant=false: got %q, want %q",
			tenant, mw.ImplicitTenant)
	}
}
