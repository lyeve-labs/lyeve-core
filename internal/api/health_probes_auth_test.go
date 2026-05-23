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
)

// /health and /ready are gated behind requireAuth on both the admin router
// (/api/admin) and the API router (/api/v1), because they report database
// state and schema count. Unauthenticated requests return 401.
func TestHealthReadyRequireAuth(t *testing.T) {
	// requireAuth rejects before the pool is reached (nil pool OK), the same
	// pattern metrics_auth_test.go uses.

	t.Run("admin /health unauthenticated returns 401", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/health", healthHandlerFn(nil))
			})
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"unauthenticated GET /api/admin/health should return 401")
	})

	t.Run("admin /ready unauthenticated returns 401", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/ready", readyHandlerFn(nil, nil))
			})
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/ready", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"unauthenticated GET /api/admin/ready should return 401")
	})

	t.Run("api/v1 /health unauthenticated returns 401", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/v1", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/health", healthHandlerFn(nil))
			})
		})

		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"unauthenticated GET /api/v1/health should return 401")
	})

	t.Run("api/v1 /ready unauthenticated returns 401", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/v1", func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/ready", readyHandlerFn(nil, nil))
			})
		})

		req := httptest.NewRequest(http.MethodGet, "/api/v1/ready", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code,
			"unauthenticated GET /api/v1/ready should return 401")
	})

	t.Run("admin /health authenticated passes auth gate", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/admin", func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					ctx := context.WithValue(req.Context(), auth.ClaimsKey,
						&auth.Claims{
							UserID: uuid.New().String(),
							Email:  "admin@test.com",
							Roles:  []string{"admin"},
						})
					next.ServeHTTP(w, req.WithContext(ctx))
				})
			})
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
			})
		})

		req := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"authenticated GET /api/admin/health must pass requireAuth")
	})

	t.Run("api/v1 /health authenticated passes auth gate", func(t *testing.T) {
		r := chi.NewRouter()
		r.Route("/api/v1", func(r chi.Router) {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					ctx := context.WithValue(req.Context(), auth.ClaimsKey,
						&auth.Claims{
							UserID: uuid.New().String(),
							Email:  "user@test.com",
							Roles:  []string{"editor"},
						})
					next.ServeHTTP(w, req.WithContext(ctx))
				})
			})
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
			})
		})

		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code,
			"authenticated GET /api/v1/health must pass requireAuth")
	})
}
