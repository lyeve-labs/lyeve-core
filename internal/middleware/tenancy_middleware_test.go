package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// TenantIDFromContext / TenantIDFromCtx

func TestTenantIDFromContext_Empty(t *testing.T) {
	if got := mw.TenantIDFromContext(context.Background()); got != "" {
		t.Errorf("empty ctx: got %q, want empty", got)
	}
}

func TestTenantIDFromContext_UnrelatedValue(t *testing.T) {
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, "acme")
	if got := mw.TenantIDFromContext(ctx); got != "" {
		t.Errorf("unrelated value: got %q, want empty", got)
	}
}

func TestTenantIDFromCtx_NoTenant(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := mw.TenantIDFromCtx(req); got != "" {
		t.Errorf("no tenant: got %q, want empty", got)
	}
}

// TenantHeader: multiTenant=false

// TestTenantHeader_SingleTenantResolvesTheImplicitTenant pins that an anonymous
// request on a single-tenant install resolves the implicit tenant. Public
// plugin routes gate on core.RequireTenantID, because sys_* tables are
// isolated by column, so they need a tenant to gate on.
//
// An authenticated request on the same install resolves the same slug. Rows
// written through the admin API carry it, so a public read matches them.
func TestTenantHeader_SingleTenantResolvesTheImplicitTenant(t *testing.T) {
	m := mw.TenantHeader(false)
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if seen != mw.ImplicitTenant {
		t.Errorf("tenant = %q, want %q", seen, mw.ImplicitTenant)
	}
}

// TenantHeader: JWT claim resolution

func TestTenantHeader_JWTClaim(t *testing.T) {
	m := mw.TenantHeader(true)
	var capturedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTenant = mw.TenantIDFromCtx(r)
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

	if capturedTenant != "acme" {
		t.Errorf("TenantIDFromCtx = %q, want %q", capturedTenant, "acme")
	}
}

func TestTenantHeader_NoJWTClaim(t *testing.T) {
	// When there are no claims, TenantHeader is a no-op.
	m := mw.TenantHeader(true)
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := mw.TenantIDFromCtx(r); got != "" {
			t.Errorf("tenant present with no claims: %q", got)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if !called {
		t.Error("next handler was not called")
	}
}

func TestTenantHeader_NilClaims(t *testing.T) {
	// Nil claims in context: no-op.
	m := mw.TenantHeader(true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := mw.TenantIDFromCtx(r); got != "" {
			t.Errorf("tenant present with nil claims: %q", got)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, (*auth.Claims)(nil))
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)
}

// TenantHeader: X-Tenant-ID override (super_admin)

func TestTenantHeader_SuperAdminOverride(t *testing.T) {
	m := mw.TenantHeader(true)
	var capturedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "override_slug")
	claims := &auth.Claims{
		TenantID: "jwt-slug",
		Roles:    []string{"super_admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if capturedTenant != "override_slug" {
		t.Errorf("super_admin override: got %q, want %q", capturedTenant, "override_slug")
	}
}

func TestTenantHeader_NonSuperAdminCannotOverride(t *testing.T) {
	// Non-super_admin roles must not be able to override via X-Tenant-ID.
	m := mw.TenantHeader(true)
	var capturedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "evil_override")
	claims := &auth.Claims{
		TenantID: "legit-tenant",
		Roles:    []string{"admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if capturedTenant != "legit-tenant" {
		t.Errorf("non-super_admin: got %q, want %q", capturedTenant, "legit-tenant")
	}
}

func TestTenantHeader_SuperAdminEmptyHeaderFallsBack(t *testing.T) {
	// When X-Tenant-ID is empty, super_admin falls back to JWT claim.
	m := mw.TenantHeader(true)
	var capturedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedTenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No X-Tenant-ID header set.
	claims := &auth.Claims{
		TenantID: "jwt-tenant",
		Roles:    []string{"super_admin"},
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	m(next).ServeHTTP(w, req)

	if capturedTenant != "jwt-tenant" {
		t.Errorf("super_admin fallback: got %q, want %q", capturedTenant, "jwt-tenant")
	}
}

// TenantHeader: JWT has no TenantID

func TestTenantHeader_JWTNoTenantID(t *testing.T) {
	// No tenant is ever invented for a claim that carries none. What differs is
	// who may proceed without one: a super_admin holds the cross-tenant scope,
	// and every other role is refused, because the empty tenant is that scope
	// (core.ResolveTenantScope).
	m := mw.TenantHeader(true)

	t.Run("super_admin proceeds with no tenant", func(t *testing.T) {
		var called bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if got := mw.TenantIDFromCtx(r); got != "" {
				t.Errorf("tenant present when JWT has no TenantID: %q", got)
			}
		})

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		claims := &auth.Claims{
			Roles: []string{"super_admin"},
			// TenantID intentionally empty
		}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

		w := httptest.NewRecorder()
		m(next).ServeHTTP(w, req)

		if !called {
			t.Error("next handler was not called")
		}
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})

	t.Run("admin is refused", func(t *testing.T) {
		var called bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		claims := &auth.Claims{Roles: []string{"admin"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

		w := httptest.NewRecorder()
		m(next).ServeHTTP(w, req)

		if called {
			t.Error("handler ran for a request that resolves to no tenant")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
}

// TenancyConn: nil / no-tenant cases

// TenancyConn is a pass-through when it has nothing to isolate with. Each case
// asserts the request reached the handler, so a middleware that swallowed the
// request, or that reached for a nil database, fails here.
func TestTenancyConn_PassesThroughWithNothingToIsolateWith(t *testing.T) {
	cases := []struct {
		name    string
		db      db.DB
		tenancy db.Tenancy
		req     func() *http.Request
	}{
		{
			name: "no database",
			req:  func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) },
		},
		{
			name:    "no tenancy strategy",
			tenancy: nil,
			req:     func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) },
		},
		{
			name: "no tenant on the request",
			req: func() *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if got := db.LazyTenantConnFromCtx(r.Context()); got != nil {
					t.Error("a lazy tenant connection was put on a request with nothing to scope it to")
				}
				w.WriteHeader(http.StatusNoContent)
			})

			rec := httptest.NewRecorder()
			mw.TenancyConn(tc.db, tc.tenancy)(next).ServeHTTP(rec, tc.req())

			if !reached {
				t.Fatal("the handler was never called; the middleware swallowed the request")
			}
			if rec.Code != http.StatusNoContent {
				t.Errorf("status %d; want the handler's own 204", rec.Code)
			}
		})
	}
}
