package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
)

// TenantHeader on /api/admin resolves the JWT tenant_id and injects it
// into context, so plugin conditional guards work.
func TestAdminRouter_TenantHeaderVerifiesTenantScopedAdmins(t *testing.T) {
	tests := []struct {
		name         string
		claims       *auth.Claims
		headerTenant string // X-Tenant-ID header
		wantTenantID string // expected TenantIDFromCtx result
		wantStatus   int
	}{
		{
			name: "tenant_scoped_admin_sees_their_tenant",
			claims: &auth.Claims{
				UserID:   "user-1",
				Roles:    []string{"admin"},
				TenantID: "tenant_alpha",
			},
			wantTenantID: "tenant_alpha",
			wantStatus:   http.StatusOK,
		},
		{
			name: "super_admin_without_tenant_sees_empty",
			claims: &auth.Claims{
				UserID:   "super-1",
				Roles:    []string{"super_admin"},
				TenantID: "", // super_admin typically has no tenant claim
			},
			wantTenantID: "",
			wantStatus:   http.StatusOK,
		},
		{
			name: "super_admin_with_tenant_override_sees_override",
			claims: &auth.Claims{
				UserID:   "super-1",
				Roles:    []string{"super_admin"},
				TenantID: "tenant_alpha",
			},
			headerTenant: "tenant_beta",
			wantTenantID: "tenant_beta",
			wantStatus:   http.StatusOK,
		},
		{
			name: "tenant_scoped_admin_header_override_ignored",
			claims: &auth.Claims{
				UserID:   "user-1",
				Roles:    []string{"admin"},
				TenantID: "tenant_alpha",
			},
			headerTenant: "tenant_beta",
			wantTenantID: "tenant_alpha", // non-super can't override
			wantStatus:   http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()

			// Simulate the admin route group middleware stack:
			// jwtAuth -> TenantHeader -> test handler
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tt.claims != nil {
						r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, tt.claims))
					}
					next.ServeHTTP(w, r)
				})
			})
			r.Use(apimw.TenantHeader(true))
			r.Get("/api/admin/test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tid := core.TenantIDFromCtx(r.Context())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"tenant_id":"` + tid + `"}`))
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
			if tt.headerTenant != "" {
				req.Header.Set("X-Tenant-ID", tt.headerTenant)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), `"tenant_id":"`+tt.wantTenantID+`"`,
				"expected tenant_id %q in body", tt.wantTenantID)
		})
	}
}

// When a tenant validator is wired via WithTenantValidator, super_admin
// override with an invalid tenant slug returns 404.
func TestAdminRouter_TenantHeaderWithValidator(t *testing.T) {
	r := chi.NewRouter()

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := &auth.Claims{
				UserID:   "super-1",
				Roles:    []string{"super_admin"},
				TenantID: "tenant_alpha",
			}
			r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, claims))
			next.ServeHTTP(w, r)
		})
	})

	// Validator that only accepts "tenant_alpha"
	validator := core.TenantValidatorFunc(func(_ context.Context, slug string) bool {
		return slug == "tenant_alpha"
	})
	r.Use(apimw.TenantHeader(true, validator))
	r.Get("/api/admin/test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := core.TenantIDFromCtx(r.Context())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tenant_id":"` + tid + `"}`))
	}))

	t.Run("valid_tenant_override_succeeds", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
		req.Header.Set("X-Tenant-ID", "tenant_alpha")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("invalid_tenant_override_returns_404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
		req.Header.Set("X-Tenant-ID", "nonexistent_tenant")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("no_override_still_resolves_jwt_tenant", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"tenant_id":"tenant_alpha"`)
	})
}

func TestAdminRouter_TenantHeaderConfigControlled(t *testing.T) {
	tests := []struct {
		name         string
		multiTenant  bool
		claims       *auth.Claims
		wantTenantID string
	}{
		{
			name:        "multi_tenant_enabled_injects_tenant",
			multiTenant: true,
			claims: &auth.Claims{
				UserID:   "admin-1",
				Roles:    []string{"admin"},
				TenantID: "tenant-gamma",
			},
			wantTenantID: "tenant-gamma",
		},
		{
			name:        "multi_tenant_disabled_still_extracts_jwt_tenant",
			multiTenant: false,
			claims: &auth.Claims{
				UserID:   "admin-1",
				Roles:    []string{"admin"},
				TenantID: "tenant-gamma",
			},
			wantTenantID: "tenant-gamma",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := chi.NewRouter()
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tt.claims != nil {
						r = r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, tt.claims))
					}
					next.ServeHTTP(w, r)
				})
			})

			r.Use(apimw.TenantHeader(tt.multiTenant))

			r.Get("/api/admin/test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tid := core.TenantIDFromCtx(r.Context())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"tenant_id":"` + tid + `"}`))
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Contains(t, rec.Body.String(), `"tenant_id":"`+tt.wantTenantID+`"`)
		})
	}
}

// Public admin routes with no JWT claims still work: TenantHeader
// passes through when no claims are present.
func TestAdminRouter_TenantHeaderPreservesNoAuthFlow(t *testing.T) {
	r := chi.NewRouter()
	r.Use(apimw.TenantHeader(true))
	r.Get("/api/admin/setup", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := core.TenantIDFromCtx(r.Context())
		assert.Empty(t, tid, "no auth -> no tenant")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
