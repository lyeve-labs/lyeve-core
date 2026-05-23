package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// An API key on the admin router is held to the scope the path names, on a
// plugin's authenticated route exactly as on the kernel's own. The key's
// scopes come from the request, so one router answers every case.
func TestAdminRouter_AuthRoutesHoldAPIKeysToTheirScope(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	routes := []plugin.PluginRoutes{{
		Name: "widgets",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/admin/widgets", Group: plugin.GroupAuth, Handler: ok},
			{Method: http.MethodPost, Pattern: "/api/admin/widgets", Group: plugin.GroupAuth, Handler: ok},
		},
	}}

	injectKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scopes := []string{}
			if s := r.Header.Get("X-Test-Scopes"); s != "" {
				scopes = strings.Split(s, ",")
			}
			claims := &core.AuthClaims{
				UserID:   uuid.NewString(),
				Roles:    []string{"viewer"},
				Scopes:   scopes,
				IsAPIKey: true,
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(),
		WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectKey), WithPluginRoutes(routes))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	cases := []struct {
		name, method, path, scopes string
		want                       int
	}{
		{"plugin route refuses a key scoped elsewhere", http.MethodGet, "/api/admin/widgets", "content:read", http.StatusForbidden},
		{"plugin route admits the key its path names", http.MethodGet, "/api/admin/widgets", "widgets:read", http.StatusOK},
		{"plugin route refuses a read scope on a write", http.MethodPost, "/api/admin/widgets", "widgets:read", http.StatusForbidden},
		{"plugin route admits a write scope on a write", http.MethodPost, "/api/admin/widgets", "widgets:write", http.StatusOK},
		{"plugin route refuses a key with no scopes", http.MethodGet, "/api/admin/widgets", "", http.StatusForbidden},
		{"plugin route admits the wildcard scope", http.MethodGet, "/api/admin/widgets", "*:*", http.StatusOK},
		{"engine route refuses a key scoped elsewhere", http.MethodGet, "/api/admin/plugins/running", "content:read", http.StatusForbidden},
		{"engine route admits the key its path names", http.MethodGet, "/api/admin/plugins/running", "plugins:read", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			if tc.scopes != "" {
				req.Header.Set("X-Test-Scopes", tc.scopes)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%s %s with scopes %q: status = %d, want %d; body=%q",
					tc.method, tc.path, tc.scopes, w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusForbidden && !strings.Contains(w.Body.String(), "insufficient scope") {
				t.Fatalf("%s %s: body = %q, want the insufficient scope refusal", tc.method, tc.path, w.Body.String())
			}
		})
	}
}

// A signed-in session carries no scopes, and the scope gate on the plugin's
// authenticated routes leaves it to the role gates.
func TestAdminRouter_AuthRoutesLeaveSessionsToTheirRole(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	routes := []plugin.PluginRoutes{{
		Name:   "widgets",
		Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/widgets", Group: plugin.GroupAuth, Handler: ok}},
	}}
	injectSession := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := &core.AuthClaims{UserID: uuid.NewString(), Roles: []string{"viewer"}}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(),
		WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectSession), WithPluginRoutes(routes))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/admin/widgets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/widgets as a session: status = %d, want 200; body=%q", w.Code, w.Body.String())
	}
}

// scopeTestRoutes are authenticated plugin routes under /api/v1: two that
// declare nothing and so take the scope their path names, one that declares
// a read scope for a POST, and one whose declared scope cannot be read.
func scopeTestRoutes() []plugin.PluginRoutes {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return []plugin.PluginRoutes{{
		Name: "widgets",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodGet, Pattern: "/api/v1/widgets", Group: plugin.GroupAuth, Handler: ok},
			{Method: http.MethodPost, Pattern: "/api/v1/widgets", Group: plugin.GroupAuth, Handler: ok},
			{Method: http.MethodPost, Pattern: "/api/v1/widgets/query", Group: plugin.GroupAuth, Handler: ok, Scope: "widgets:read"},
			{Method: http.MethodGet, Pattern: "/api/v1/widgets/broken", Group: plugin.GroupAuth, Handler: ok, Scope: "widgets"},
		},
	}}
}

// injectTestClaims stands in for the credential middleware: X-Test-Scopes
// makes the caller an API key with those scopes, X-Test-Roles sets its
// roles, and X-Test-Session makes it a signed-in session instead.
func injectTestClaims(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		roles := []string{"viewer"}
		if s := r.Header.Get("X-Test-Roles"); s != "" {
			roles = strings.Split(s, ",")
		}
		claims := &core.AuthClaims{UserID: uuid.NewString(), Roles: roles}
		if r.Header.Get("X-Test-Session") == "" {
			claims.IsAPIKey = true
			claims.Scopes = []string{}
			if s := r.Header.Get("X-Test-Scopes"); s != "" {
				claims.Scopes = strings.Split(s, ",")
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
	})
}

// An API key on the API router is held to the scope a plugin's
// authenticated route declares, or to the one its path names when it
// declares none, exactly as the admin router holds it.
func TestAPIRouter_AuthRoutesHoldAPIKeysToTheirScope(t *testing.T) {
	t.Parallel()

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil,
		WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectTestClaims), WithPluginRoutes(scopeTestRoutes()))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	cases := []struct {
		name, method, path string
		headers            map[string]string
		want               int
	}{
		{"a route declaring nothing refuses a key scoped elsewhere", http.MethodGet, "/api/v1/widgets", map[string]string{"X-Test-Scopes": "content:read"}, http.StatusForbidden},
		{"a route declaring nothing refuses a key with no scopes", http.MethodGet, "/api/v1/widgets", nil, http.StatusForbidden},
		{"a route declaring nothing admits the key its path names", http.MethodGet, "/api/v1/widgets", map[string]string{"X-Test-Scopes": "widgets:read"}, http.StatusOK},
		{"a route declaring nothing refuses a read scope on a write", http.MethodPost, "/api/v1/widgets", map[string]string{"X-Test-Scopes": "widgets:read"}, http.StatusForbidden},
		{"a route declaring nothing admits a write scope on a write", http.MethodPost, "/api/v1/widgets", map[string]string{"X-Test-Scopes": "widgets:write"}, http.StatusOK},
		{"a route declaring nothing admits a resource wildcard", http.MethodPost, "/api/v1/widgets", map[string]string{"X-Test-Scopes": "widgets:*"}, http.StatusOK},
		{"a route declaring nothing admits the full wildcard", http.MethodGet, "/api/v1/widgets", map[string]string{"X-Test-Scopes": "*:*"}, http.StatusOK},
		{"a declared scope admits a key holding it", http.MethodPost, "/api/v1/widgets/query", map[string]string{"X-Test-Scopes": "widgets:read"}, http.StatusOK},
		{"a declared scope admits an action wildcard", http.MethodPost, "/api/v1/widgets/query", map[string]string{"X-Test-Scopes": "*:read"}, http.StatusOK},
		{"a declared scope replaces the one its path names", http.MethodPost, "/api/v1/widgets/query", map[string]string{"X-Test-Scopes": "widgets:write"}, http.StatusForbidden},
		{"a declared scope refuses a key scoped elsewhere", http.MethodPost, "/api/v1/widgets/query", map[string]string{"X-Test-Scopes": "content:read"}, http.StatusForbidden},
		{"an unreadable declared scope refuses the scope it spells", http.MethodGet, "/api/v1/widgets/broken", map[string]string{"X-Test-Scopes": "widgets:read,widgets:*"}, http.StatusForbidden},
		{"an unreadable declared scope admits only the full wildcard", http.MethodGet, "/api/v1/widgets/broken", map[string]string{"X-Test-Scopes": "*:*"}, http.StatusOK},
		{"a key holding the admin role is held to its role", http.MethodPost, "/api/v1/widgets", map[string]string{"X-Test-Roles": "admin"}, http.StatusOK},
		{"a session is left to the role gates", http.MethodPost, "/api/v1/widgets", map[string]string{"X-Test-Session": "1"}, http.StatusOK},
		{"a session reaches a route with an unreadable scope", http.MethodGet, "/api/v1/widgets/broken", map[string]string{"X-Test-Session": "1"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%s %s with %v: status = %d, want %d; body=%q",
					tc.method, tc.path, tc.headers, w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusForbidden && !strings.Contains(w.Body.String(), "insufficient scope") {
				t.Fatalf("%s %s: body = %q, want the insufficient scope refusal", tc.method, tc.path, w.Body.String())
			}
		})
	}
}

// An admin token is for the admin API, so the API router refuses it on a
// plugin's authenticated route before any scope is read, whatever the route
// declares.
func TestAPIRouter_AuthRoutesRefuseAdminTokens(t *testing.T) {
	t.Parallel()

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil,
		WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectTestClaims), WithPluginRoutes(scopeTestRoutes()))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}
	for _, path := range []string{"/api/v1/widgets", "/api/v1/widgets/broken"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+core.AdminTokenPrefix+"0123456789abcdef")
		req.Header.Set("X-Test-Scopes", "*:*")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s with an admin token: status = %d, want 401; body=%q", path, w.Code, w.Body.String())
		}
	}
}

// The admin router reads a declared scope the same way, so one declaration
// means the same thing under either prefix.
func TestAdminRouter_AuthRoutesHoldAPIKeysToTheirDeclaredScope(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	routes := []plugin.PluginRoutes{{
		Name: "widgets",
		Routes: []plugin.RouteDecl{
			{Method: http.MethodPost, Pattern: "/api/admin/widgets/query", Group: plugin.GroupAuth, Handler: ok, Scope: "widgets:read"},
		},
	}}
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(),
		WithLifetime(testLifetime(t)), WithAPIKeyAuth(injectTestClaims), WithPluginRoutes(routes))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}
	for scopes, want := range map[string]int{"widgets:read": http.StatusOK, "widgets:write": http.StatusForbidden} {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/widgets/query", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Scopes", scopes)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("POST /api/admin/widgets/query with %q: status = %d, want %d; body=%q", scopes, w.Code, want, w.Body.String())
		}
	}
}
