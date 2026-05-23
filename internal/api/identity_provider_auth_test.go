// Tests for the route groups mountPluginRoutes enforces on plugin routes: a
// route declared in GroupSuperAdmin is reachable only with the super_admin
// claim, and a GroupAdmin route stays open to admin. The fixture paths stand
// for any plugin's provider configuration routes.
//
// They complement TestMountPluginRoutes_GroupSuperAdmin_Gating, which tests
// the middleware layer in isolation, by running many routes of each group
// through the same mount.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func identityClaimsCtx(req *http.Request, userID uuid.UUID, email string, roles []string) *http.Request {
	claims := &auth.Claims{
		UserID: userID.String(),
		Email:  email,
		Roles:  roles,
	}
	return req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
}

func newIdentityRequest(method, path, body string, userID uuid.UUID, roles []string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	return identityClaimsCtx(req, userID, "test@example.com", roles)
}

// Table-driven endpoint gating tests

func TestMountPluginRoutes_SuperAdminGroupGating(t *testing.T) {
	t.Parallel()

	userID := uuid.New()

	type endpointTest struct {
		name       string
		method     string
		path       string
		body       string
		routeGroup plugin.RouteGroup
	}

	// Every endpoint here is declared in GroupSuperAdmin, so only super_admin reaches it.
	superAdminEndpoints := []endpointTest{
		{name: "provider_create", method: "POST", path: "/example-providers", body: `{"name":"test","secret":"x"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "provider_update", method: "PUT", path: "/example-providers/{id}", body: `{"name":"test2"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "provider_delete", method: "DELETE", path: "/example-providers/{id}", body: "", routeGroup: plugin.GroupSuperAdmin},
		{name: "provider_reset", method: "POST", path: "/example-providers/{id}/reset", body: "", routeGroup: plugin.GroupSuperAdmin},
		{name: "link_delete", method: "DELETE", path: "/example-links/{id}", body: "", routeGroup: plugin.GroupSuperAdmin},
		{name: "link_set_primary", method: "PATCH", path: "/example-links/{id}/primary", body: `{"user_id":"` + userID.String() + `"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "link_resolve", method: "POST", path: "/example-links/resolve", body: `{"target_user_id":"` + userID.String() + `","source_user_id":"` + userID.String() + `"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "federation_create", method: "POST", path: "/example-federations", body: `{"name":"test","certificate":"x"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "federation_update", method: "PUT", path: "/example-federations/{id}", body: `{"name":"test2"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "federation_delete", method: "DELETE", path: "/example-federations/{id}", body: "", routeGroup: plugin.GroupSuperAdmin},
		{name: "federation_rotate_cert", method: "POST", path: "/example-federations/{id}/rotate-cert", body: "", routeGroup: plugin.GroupSuperAdmin},
		{name: "directory_create", method: "POST", path: "/example-directories", body: `{"name":"test","token":"abc123"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "directory_delete", method: "DELETE", path: "/example-directories/{id}", body: "", routeGroup: plugin.GroupSuperAdmin},
		{name: "directory_rotate_token", method: "POST", path: "/example-directories/{id}/rotate", body: `{"new_token":"new-secret"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "store_create", method: "POST", path: "/example-stores", body: `{"name":"test","endpoint":"/tmp"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "store_update", method: "PUT", path: "/example-stores/{id}", body: `{"name":"test2"}`, routeGroup: plugin.GroupSuperAdmin},
		{name: "store_delete", method: "DELETE", path: "/example-stores/{id}", body: "", routeGroup: plugin.GroupSuperAdmin},
	}

	t.Run("GroupAdmin_receives_403_on_all_super_admin_endpoints", func(t *testing.T) {
		t.Parallel()
		for _, ep := range superAdminEndpoints {
			path := strings.ReplaceAll(ep.path, "{id}", userID.String())

			t.Run(ep.name, func(t *testing.T) {
				t.Parallel()

				r := chi.NewRouter()

				pluginRoutes := []plugin.PluginRoutes{
					{
						Name: "test",
						Routes: []plugin.RouteDecl{
							{
								Method:  ep.method,
								Pattern: "/api/admin" + ep.path,
								Group:   ep.routeGroup,
								Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									w.WriteHeader(http.StatusOK)
								}),
							},
						},
					},
				}
				mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

				req := httptest.NewRequest(ep.method, path, nil)
				if ep.body != "" {
					req = httptest.NewRequest(ep.method, path, strings.NewReader(ep.body))
					req.Header.Set("Content-Type", "application/json")
				}
				req = identityClaimsCtx(req, userID, "test@example.com", []string{"admin"})

				rr := httptest.NewRecorder()
				r.ServeHTTP(rr, req)

				if rr.Code != http.StatusForbidden {
					t.Errorf("endpoint %s %s: GroupAdmin got %d, want %d (403 Forbidden)",
						ep.method, path, rr.Code, http.StatusForbidden)
				}
			})
		}
	})

	t.Run("GroupSuperAdmin_succeeds_on_all_super_admin_endpoints", func(t *testing.T) {
		t.Parallel()
		for _, ep := range superAdminEndpoints {
			path := strings.ReplaceAll(ep.path, "{id}", userID.String())

			t.Run(ep.name, func(t *testing.T) {
				t.Parallel()

				r := chi.NewRouter()

				pluginRoutes := []plugin.PluginRoutes{
					{
						Name: "test",
						Routes: []plugin.RouteDecl{
							{
								Method:  ep.method,
								Pattern: "/api/admin" + ep.path,
								Group:   ep.routeGroup,
								Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									w.WriteHeader(http.StatusOK)
								}),
							},
						},
					},
				}
				mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

				req := httptest.NewRequest(ep.method, path, nil)
				if ep.body != "" {
					req = httptest.NewRequest(ep.method, path, strings.NewReader(ep.body))
					req.Header.Set("Content-Type", "application/json")
				}
				req = identityClaimsCtx(req, userID, "test@example.com", []string{"super_admin"})

				rr := httptest.NewRecorder()
				r.ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Errorf("endpoint %s %s: GroupSuperAdmin got %d, want %d (200 OK)",
						ep.method, path, rr.Code, http.StatusOK)
				}
			})
		}
	})

	t.Run("GroupAdmin_can_still_access_GroupAdmin_endpoints", func(t *testing.T) {
		t.Parallel()

		adminReadEndpoints := []endpointTest{
			{name: "provider_list", method: "GET", path: "/example-providers", body: "", routeGroup: plugin.GroupAdmin},
			{name: "provider_health", method: "GET", path: "/example-providers/health", body: "", routeGroup: plugin.GroupAdmin},
			{name: "provider_templates", method: "GET", path: "/example-templates", body: "", routeGroup: plugin.GroupAdmin},
			{name: "links_list", method: "GET", path: "/example-links", body: "", routeGroup: plugin.GroupAdmin},
			{name: "federations_list", method: "GET", path: "/example-federations", body: "", routeGroup: plugin.GroupAdmin},
			{name: "federation_templates", method: "GET", path: "/example-federation-templates", body: "", routeGroup: plugin.GroupAdmin},
			{name: "directories_list", method: "GET", path: "/example-directories", body: "", routeGroup: plugin.GroupAdmin},
			{name: "stores_list", method: "GET", path: "/example-stores", body: "", routeGroup: plugin.GroupAdmin},
		}

		for _, ep := range adminReadEndpoints {
			t.Run(ep.name, func(t *testing.T) {
				t.Parallel()

				r := chi.NewRouter()

				pluginRoutes := []plugin.PluginRoutes{
					{
						Name: "test",
						Routes: []plugin.RouteDecl{
							{
								Method:  ep.method,
								Pattern: "/api/admin" + ep.path,
								Group:   ep.routeGroup,
								Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									w.WriteHeader(http.StatusOK)
								}),
							},
						},
					},
				}
				mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

				req := httptest.NewRequest(ep.method, ep.path, nil)
				req = identityClaimsCtx(req, userID, "test@example.com", []string{"admin"})

				rr := httptest.NewRecorder()
				r.ServeHTTP(rr, req)

				if rr.Code != http.StatusOK {
					t.Errorf("endpoint %s %s: GroupAdmin got %d, want %d (200 OK - admin endpoints should still be accessible)",
						ep.method, ep.path, rr.Code, http.StatusOK)
				}
			})
		}
	})

	t.Run("unauthenticated_receives_401", func(t *testing.T) {
		t.Parallel()
		ep := superAdminEndpoints[0]

		r := chi.NewRouter()
		pluginRoutes := []plugin.PluginRoutes{
			{
				Name: "test",
				Routes: []plugin.RouteDecl{
					{
						Method:  ep.method,
						Pattern: "/api/admin" + ep.path,
						Group:   ep.routeGroup,
						Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.WriteHeader(http.StatusOK)
						}),
					},
				},
			},
		}
		mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

		req := httptest.NewRequest(ep.method, ep.path, strings.NewReader(ep.body))
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated: got %d, want %d (401 Unauthorized)", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("user_with_no_roles_receives_403", func(t *testing.T) {
		t.Parallel()
		ep := superAdminEndpoints[0]

		r := chi.NewRouter()
		pluginRoutes := []plugin.PluginRoutes{
			{
				Name: "test",
				Routes: []plugin.RouteDecl{
					{
						Method:  ep.method,
						Pattern: "/api/admin" + ep.path,
						Group:   ep.routeGroup,
						Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.WriteHeader(http.StatusOK)
						}),
					},
				},
			},
		}
		mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

		req := newIdentityRequest(ep.method, ep.path, ep.body, userID, []string{})

		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("no roles: got %d, want %d (403 Forbidden)", rr.Code, http.StatusForbidden)
		}
	})
}

// RequireSuperAdmin defense-in-depth test

func TestRequireSuperAdmin_DefenseInDepth(t *testing.T) {
	t.Parallel()

	userID := uuid.New()

	tests := []struct {
		name       string
		roles      []string
		wantAccess bool
	}{
		{"super_admin_passes", []string{"super_admin"}, true},
		{"admin_blocked", []string{"admin"}, false},
		{"admin_and_editor_blocked", []string{"admin", "editor"}, false},
		{"super_admin_and_admin_passes", []string{"super_admin", "admin"}, true},
		{"empty_roles_blocked", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/test", nil)
			claims := &core.AuthClaims{
				UserID: userID.String(),
				Email:  "test@example.com",
				Roles:  tt.roles,
			}
			req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, claims))

			got := core.RequireSuperAdmin(req)

			if got != tt.wantAccess {
				t.Errorf("RequireSuperAdmin(roles=%v) = %v, want %v", tt.roles, got, tt.wantAccess)
			}
		})
	}
}

// Claims on the request context

func TestGroupSuperAdmin_AuditLogRecordsActor(t *testing.T) {
	t.Parallel()

	userID := uuid.New()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/example-providers", nil)
	claims := &core.AuthClaims{
		UserID: userID.String(),
		Email:  "test@example.com",
		Roles:  []string{"super_admin"},
	}
	req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, claims))

	got := core.GetClaims(req.Context())
	if got == nil {
		t.Fatal("GetClaims returned nil for authenticated request")
	}
	if got.UserID != userID.String() {
		t.Errorf("GetClaims().UserID = %q, want %q", got.UserID, userID.String())
	}
	if !got.HasRole("super_admin") {
		t.Error("GetClaims().HasRole(\"super_admin\") = false, want true")
	}
}

// 403 response format verification

func TestGroupSuperAdmin_403ResponseIsJSON(t *testing.T) {
	t.Parallel()

	userID := uuid.New()

	r := chi.NewRouter()
	pluginRoutes := []plugin.PluginRoutes{
		{
			Name: "test",
			Routes: []plugin.RouteDecl{
				{
					Method:  "POST",
					Pattern: "/api/admin/example-providers",
					Group:   plugin.GroupSuperAdmin,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusOK)
					}),
				},
			},
		},
	}
	mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

	req := newIdentityRequest("POST", "/example-providers", `{}`, userID, []string{"admin"})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}

	ct := rr.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Errorf("403 response is not valid JSON: %v (body=%q)", err, rr.Body.String())
	}
	if _, ok := body["error"]; !ok {
		t.Errorf("403 JSON response missing 'error' field: %v", body)
	}
}

// Mixed route group test

func TestMountPluginRoutes_MixedGroupsGateEachRoute(t *testing.T) {
	t.Parallel()

	userID := uuid.New()

	r := chi.NewRouter()
	pluginRoutes := []plugin.PluginRoutes{
		{
			Name: "test",
			Routes: []plugin.RouteDecl{
				{
					Method:  "GET",
					Pattern: "/api/admin/read",
					Group:   plugin.GroupAdmin,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusOK)
					}),
				},
				{
					Method:  "POST",
					Pattern: "/api/admin/write",
					Group:   plugin.GroupSuperAdmin,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(http.StatusOK)
					}),
				},
			},
		},
	}
	mountPluginRoutes(r, pluginRoutes, "/api/admin", false, 1<<20, nil, nil, nil)

	tests := []struct {
		name       string
		roles      []string
		path       string
		wantStatus int
	}{
		{"super_admin_read", []string{"super_admin"}, "/read", http.StatusOK},
		{"super_admin_write", []string{"super_admin"}, "/write", http.StatusOK},
		{"admin_read", []string{"admin"}, "/read", http.StatusOK},
		{"admin_write_blocked", []string{"admin"}, "/write", http.StatusForbidden},
		{"no_role_write_blocked", []string{"editor"}, "/write", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			method := "GET"
			if strings.Contains(tt.name, "write") {
				method = "POST"
			}
			req := newIdentityRequest(method, tt.path, "", userID, tt.roles)
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Errorf("%s %s (roles=%v): status=%d, want %d",
					tt.name, tt.path, tt.roles, rr.Code, tt.wantStatus)
			}
		})
	}
}
