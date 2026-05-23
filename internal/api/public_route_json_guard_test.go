// Public-route jsonGuard integration tests.
//
// Verifies mountPluginRoutes applies ContentLengthLimit (jsonGuard) to public
// POST, PUT and PATCH routes too, matching the auth, admin and super admin
// groups.

package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Public routes are wrapped by ContentLengthLimit middleware (jsonGuard), so
// a per-route Content-Length check applies beneath the global MaxBodySize.
func TestPublicRoutes_JSONGuard(t *testing.T) {
	t.Parallel()

	const maxJSONBytes int64 = 100

	routes := []plugin.PluginRoutes{{
		Name: "test-public",
		Routes: []plugin.RouteDecl{
			{
				Method:  "POST",
				Pattern: "/api/admin/test/public-post",
				Group:   plugin.GroupPublic,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			},
			{
				Method:  "PUT",
				Pattern: "/api/admin/test/public-put",
				Group:   plugin.GroupPublic,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			},
			{
				// GET should route correctly alongside POST/PUT even though
				// ContentLengthLimit targets methods with bodies.
				Method:  "GET",
				Pattern: "/api/admin/test/public-get",
				Group:   plugin.GroupPublic,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}),
			},
		},
	}}

	r := chi.NewRouter()
	mountPluginRoutes(r, routes, "/api/admin", false, maxJSONBytes, nil, nil, nil)

	tests := []struct {
		name        string
		method      string
		path        string
		contentLen  int64
		wantStatus  int
		wantBlocked bool
	}{
		{
			name:        "post_small_body_passes",
			method:      http.MethodPost,
			path:        "/test/public-post",
			contentLen:  50,
			wantStatus:  http.StatusOK,
			wantBlocked: false,
		},
		{
			name:        "post_large_body_blocked",
			method:      http.MethodPost,
			path:        "/test/public-post",
			contentLen:  200,
			wantStatus:  http.StatusRequestEntityTooLarge,
			wantBlocked: true,
		},
		{
			name:        "put_small_body_passes",
			method:      http.MethodPut,
			path:        "/test/public-put",
			contentLen:  50,
			wantStatus:  http.StatusOK,
			wantBlocked: false,
		},
		{
			name:        "put_large_body_blocked",
			method:      http.MethodPut,
			path:        "/test/public-put",
			contentLen:  200,
			wantStatus:  http.StatusRequestEntityTooLarge,
			wantBlocked: true,
		},
		{
			name:        "get_also_blocked_with_large_body",
			method:      http.MethodGet,
			path:        "/test/public-get",
			contentLen:  200,
			wantStatus:  http.StatusRequestEntityTooLarge,
			wantBlocked: true,
		},
		{
			name:        "post_exactly_at_limit_passes",
			method:      http.MethodPost,
			path:        "/test/public-post",
			contentLen:  100,
			wantStatus:  http.StatusOK,
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), int(tt.contentLen))
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader(payload))
			req.ContentLength = tt.contentLen

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// The core auth endpoints are mounted directly on the public group rather than
// through mountPluginRoutes, so they carry their own per-route cap. They are
// reachable without a credential and need it most.
func TestPublicAuthRoutes_JSONGuard(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	oversize := cfg.MaxJSONBodyBytes + 1

	adminRouter, err := NewAdminRouter(&fakeDB{engine: "postgres"}, cfg, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	apiRouter, err := NewAPIRouter(&fakeDB{engine: "postgres"}, cfg, nil, WithLifetime(testLifetime(t)))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	cases := []struct {
		path   string
		router http.Handler
	}{
		{"/api/admin/setup", adminRouter},
		{"/api/admin/auth/login", adminRouter},
		{"/api/admin/auth/refresh", adminRouter},
		{"/api/admin/auth/mfa-verify", adminRouter},
		{"/api/v1/auth/token", apiRouter},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path,
				bytes.NewReader(bytes.Repeat([]byte("x"), int(oversize))))
			req.Header.Set("Content-Type", "application/json")
			req.ContentLength = oversize

			rec := httptest.NewRecorder()
			tc.router.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("POST %s with %d bytes: status = %d, want %d",
					tc.path, oversize, rec.Code, http.StatusRequestEntityTooLarge)
			}
		})
	}
}
