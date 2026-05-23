package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/metrics"
)

func metricsAuthClaims(userID uuid.UUID, email string, roles []string) *auth.Claims {
	return &auth.Claims{
		UserID: userID.String(),
		Email:  email,
		Roles:  roles,
	}
}

// /metrics is gated behind metricsAuth (metrics token OR super_admin role),
// rejecting unauthenticated and non-super_admin callers. When no metrics
// token is set, the middleware degrades to requireRole("super_admin").
func TestMetricsEndpoint_RequiresSuperAdmin(t *testing.T) {
	metrics.Init()

	t.Run("unauthorized without credentials", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.With(metricsAuth("")).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"unauthenticated GET /api/admin/metrics should return 401")
	})

	t.Run("forbidden for non-super_admin role", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					ctx := context.WithValue(req.Context(), auth.ClaimsKey,
						metricsAuthClaims(uuid.New(), "editor@test.com", []string{"editor"}))
					next.ServeHTTP(w, req.WithContext(ctx))
				})
			})
			r.With(metricsAuth("")).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code,
			"editor GET /api/admin/metrics should return 403")
	})

	t.Run("allowed for super_admin", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					ctx := context.WithValue(req.Context(), auth.ClaimsKey,
						metricsAuthClaims(uuid.New(), "admin@test.com", []string{"super_admin"}))
					next.ServeHTTP(w, req.WithContext(ctx))
				})
			})
			r.With(metricsAuth("")).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"super_admin GET /api/admin/metrics should return 200")
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain",
			"metrics output should be Prometheus text format")
	})
}

// /metrics accepts a static bearer token (METRICS_TOKEN) without a JWT,
// enabling long-lived Prometheus scraping.
func TestMetricsEndpoint_ScrapeToken(t *testing.T) {
	metrics.Init()
	const testToken = "test-metrics-token-32-chars-min!!"

	t.Run("accepts valid metrics token", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.With(metricsAuth(testToken)).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"valid metrics token should return 200")
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain",
			"metrics output should be Prometheus text format")
	})

	t.Run("rejects wrong metrics token", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.With(metricsAuth(testToken)).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		req.Header.Set("Authorization", "Bearer wrong-token-value!!")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"wrong metrics token should return 401")
	})

	t.Run("metrics token works without JWT claims", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.With(metricsAuth(testToken)).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"metrics token should work without JWT claims")
	})

	t.Run("super_admin JWT still works alongside metrics token", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					ctx := context.WithValue(req.Context(), auth.ClaimsKey,
						metricsAuthClaims(uuid.New(), "admin@test.com", []string{"super_admin"}))
					next.ServeHTTP(w, req.WithContext(ctx))
				})
			})
			r.With(metricsAuth(testToken)).Get("/metrics", metrics.Handler().ServeHTTP)
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"super_admin JWT should still work when metrics token is configured")
	})
}
