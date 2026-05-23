//go:build !mutest

// Package api: CSRF plugin route protection tests.
//
// Verifies that plugin admin routes on the admin server (:3001, cookie auth)
// are protected by the double-submit CSRF token check, while the /api/v1
// Bearer/API-key server omits CSRF.
//
// Run: go test ./internal/api/ -run TestPluginCSRF -count=1 -race
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Admin server: csrfProtect=true, CSRF enforced on plugin admin POST routes

func TestPluginCSRF_AdminRoutePOSTWithoutTokenReturns403(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/admin/test-plugin/action",
			Group:   plugin.GroupAdmin,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/action", nil)
	// Inject admin claims so requireRole("admin","super_admin") passes.
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "admin@test.com", []string{"admin"})))
	// Session cookie present: triggers CSRF check.
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// No __Host-csrf cookie, no X-CSRF-Token header: should be rejected.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF token: expected 403, got %d", rec.Code)
	}
}

func TestPluginCSRF_AdminRoutePOSTWithValidTokenSucceeds(t *testing.T) {
	token := csrfTestToken(t)

	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/admin/test-plugin/action",
			Group:   plugin.GroupAdmin,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/action", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "admin@test.com", []string{"admin"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: token})
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("POST with valid CSRF token: expected 200, got %d", rec.Code)
	}
}

func TestPluginCSRF_AdminRouteGETSkipsCSRF(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "GET",
			Pattern: "/api/admin/test-plugin/read",
			Group:   plugin.GroupAdmin,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/test-plugin/read", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "admin@test.com", []string{"admin"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// No CSRF cookie or header, but GET is a safe method, so CSRFCheck passes through.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET without CSRF token: expected 200 (safe method), got %d", rec.Code)
	}
}

// Admin server: super-admin group routes also get CSRF protection

func TestPluginCSRF_SuperAdminRoutePOSTWithoutTokenReturns403(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/admin/test-plugin/admin-action",
			Group:   plugin.GroupSuperAdmin,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/admin-action", nil)
	// Inject super_admin claims so requireRole("super_admin") passes.
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "sa@test.com", []string{"super_admin"})))
	// Session cookie present: triggers CSRF check.
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// No __Host-csrf cookie, no X-CSRF-Token header: should be rejected.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("super-admin POST without CSRF token: expected 403, got %d", rec.Code)
	}
}

func TestPluginCSRF_SuperAdminRoutePOSTWithValidTokenSucceeds(t *testing.T) {
	token := csrfTestToken(t)

	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/admin/test-plugin/admin-action",
			Group:   plugin.GroupSuperAdmin,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/admin-action", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "sa@test.com", []string{"super_admin"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: token})
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("super-admin POST with valid CSRF token: expected 200, got %d", rec.Code)
	}
}

func TestPluginCSRF_SuperAdminRouteGETSkipsCSRF(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "GET",
			Pattern: "/api/admin/test-plugin/admin-read",
			Group:   plugin.GroupSuperAdmin,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/test-plugin/admin-read", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "sa@test.com", []string{"super_admin"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// No CSRF cookie or header, but GET is a safe method, so CSRFCheck passes through.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("super-admin GET without CSRF token: expected 200 (safe method), got %d", rec.Code)
	}
}

// Admin server: auth-group (default) routes also get CSRF protection

func TestPluginCSRF_AuthGroupRoutePOSTWithoutTokenReturns403(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/admin/test-plugin/update",
			Group:   plugin.GroupAuth,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/update", nil)
	// Inject claims so requireAuth passes, then CSRFCheck runs.
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "user@test.com", []string{"editor"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// No CSRF cookie or header: should be rejected.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("auth-group POST without CSRF token: expected 403, got %d", rec.Code)
	}
}

func TestPluginCSRF_AuthGroupRouteWithValidTokenSucceeds(t *testing.T) {
	token := csrfTestToken(t)

	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/admin/test-plugin/update",
			Group:   plugin.GroupAuth,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/update", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "user@test.com", []string{"editor"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: token})
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("auth-group POST with valid CSRF: expected 200, got %d", rec.Code)
	}
}

// API server: csrfProtect=true, CSRF enforced on /api/v1 routes too

func TestPluginCSRF_APIV1RoutePOSTWithoutTokenReturns403(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/v1/test-plugin/action",
			Group:   plugin.GroupAuth,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/v1", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/action", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "user@test.com", []string{"editor"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// No CSRF cookie or header: should be rejected.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("POST on /api/v1 without CSRF token: expected 403, got %d", rec.Code)
	}
}

func TestPluginCSRF_APIV1RoutePOSTWithValidTokenSucceeds(t *testing.T) {
	token := csrfTestToken(t)

	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/v1/test-plugin/action",
			Group:   plugin.GroupAuth,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/v1", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/action", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "user@test.com", []string{"editor"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: token})
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("POST on /api/v1 with valid CSRF token: expected 200, got %d", rec.Code)
	}
}

func TestPluginCSRF_APIV1BearerTokenSkipsCSRF(t *testing.T) {
	// Requests with a Bearer token but no session cookie should bypass CSRF
	// entirely: CSRFCheck skips when no sys_session cookie is present.
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "POST",
			Pattern: "/api/v1/test-plugin/action",
			Group:   plugin.GroupAuth,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/v1", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/test-plugin/action", nil)
	req.Header.Set("Authorization", "Bearer some-valid-jwt")
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "user@test.com", []string{"editor"})))
	// No session cookie: CSRFCheck skips.
	// No CSRF token: should still pass because there's no session cookie.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("POST on /api/v1 with Bearer token and no session cookie: expected 200, got %d", rec.Code)
	}
}

func TestPluginCSRF_APIV1GETSkipsCSRF(t *testing.T) {
	routes := []plugin.PluginRoutes{{
		Name: "test-plugin",
		Routes: []plugin.RouteDecl{{
			Method:  "GET",
			Pattern: "/api/v1/test-plugin/read",
			Group:   plugin.GroupAuth,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/v1", true, 1<<20, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/test-plugin/read", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
		makeClaims(uuid.New(), "user@test.com", []string{"editor"})))
	req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
	// GET is a safe method: CSRFCheck passes through.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET on /api/v1 without CSRF token: expected 200 (safe method), got %d", rec.Code)
	}
}

// Inline /api/v1 content-write routes with session cookie get CSRF
// (the Go chi group middleware applies to all routes declared inside)

func TestPluginCSRF_InlineRouteGroup_CSRFApplied(t *testing.T) {
	// Simulate the inline /api/v1 auth group middleware stack.
	// The actual router.go /api/v1 group uses:
	//   r.Use(requireAuth)
	//   r.Use(apimw.CSRFCheck)
	//   r.Use(apimw.ContentLengthLimit(...))
	//   ...
	// then declares inline routes like:
	//   r.With(requireRole("editor","admin","super_admin")).Post("/content/{schema}", ...)
	//
	// This test verifies that the CSRFCheck middleware (applied at the group
	// level) catches any POST in the group: whether plugin-route or inline.

	t.Run("inline_post_with_session_no_csrf_rejected_403", func(t *testing.T) {
		r := chi.NewRouter()
		r.Group(func(sub chi.Router) {
			sub.Use(requireAuth)
			sub.Use(apimw.CSRFCheck)
			sub.Use(apimw.ContentLengthLimit(1 << 20))
			sub.With(requireRole("editor", "admin", "super_admin")).Post("/test-inline", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
		})

		req := httptest.NewRequest(http.MethodPost, "/test-inline", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
			makeClaims(uuid.New(), "editor@test.com", []string{"editor"})))
		req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
		// No CSRF token: should reject.
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("inline POST with session cookie, no CSRF: expected 403, got %d", rec.Code)
		}
	})

	t.Run("inline_post_with_session_and_valid_csrf_succeeds_200", func(t *testing.T) {
		token := csrfTestToken(t)

		r := chi.NewRouter()
		r.Group(func(sub chi.Router) {
			sub.Use(requireAuth)
			sub.Use(apimw.CSRFCheck)
			sub.Use(apimw.ContentLengthLimit(1 << 20))
			sub.With(requireRole("editor", "admin", "super_admin")).Post("/test-inline", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
		})

		req := httptest.NewRequest(http.MethodPost, "/test-inline", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
			makeClaims(uuid.New(), "editor@test.com", []string{"editor"})))
		req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
		req.AddCookie(&http.Cookie{Name: "__Host-csrf", Value: token})
		req.Header.Set("X-CSRF-Token", token)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("inline POST with session cookie and valid CSRF: expected 200, got %d", rec.Code)
		}
	})

	t.Run("inline_post_with_bearer_token_skips_csrf_200", func(t *testing.T) {
		r := chi.NewRouter()
		r.Group(func(sub chi.Router) {
			sub.Use(requireAuth)
			sub.Use(apimw.CSRFCheck)
			sub.Use(apimw.ContentLengthLimit(1 << 20))
			sub.With(requireRole("editor", "admin", "super_admin")).Post("/test-inline", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
		})

		req := httptest.NewRequest(http.MethodPost, "/test-inline", nil)
		req.Header.Set("Authorization", "Bearer some-valid-jwt")
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
			makeClaims(uuid.New(), "editor@test.com", []string{"editor"})))
		// Bearer token, no session cookie: CSRFCheck skips.
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("inline POST with Bearer token, no session: expected 200, got %d", rec.Code)
		}
	})

	t.Run("inline_get_with_session_skips_csrf_200", func(t *testing.T) {
		r := chi.NewRouter()
		r.Group(func(sub chi.Router) {
			sub.Use(requireAuth)
			sub.Use(apimw.CSRFCheck)
			sub.Use(apimw.ContentLengthLimit(1 << 20))
			sub.With(requireRole("editor", "admin", "super_admin")).Get("/test-inline", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
		})

		req := httptest.NewRequest(http.MethodGet, "/test-inline", nil)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey,
			makeClaims(uuid.New(), "editor@test.com", []string{"editor"})))
		req.AddCookie(&http.Cookie{Name: "sys_session", Value: "fake-jwt"})
		// GET is safe: no CSRF token needed.
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("inline GET with session cookie: expected 200 (safe method), got %d", rec.Code)
		}
	})
}
