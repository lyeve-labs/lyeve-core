package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// An API key holding an admin role is refused on the admin API, where admin
// tokens are the credential. A key with any other role is not.
func TestAdminRouter_AdminRoleKeyIsRefused(t *testing.T) {
	t.Parallel()

	withKey := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := r.Header.Get("X-API-Key")
			if role == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims := &core.AuthClaims{UserID: "k-" + role, APIKeyID: "k-" + role, Roles: []string{role}, IsAPIKey: true, TenantID: "default"}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)),
		WithAPIKeyAuth(withKey),
		WithPluginRoutes([]plugin.PluginRoutes{{
			Name:   "test-plugin",
			Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/admin/test-plugin/items", Group: plugin.GroupAdmin, Handler: ok}},
		}}))
	if err != nil {
		t.Fatalf("NewAdminRouter: %v", err)
	}

	for _, tc := range []struct {
		role    string
		refused bool
	}{
		{"super_admin", true},
		{"admin", true},
		{"editor", false},
		{"", false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/test-plugin/items", nil)
		if tc.role != "" {
			req.Header.Set("X-API-Key", tc.role)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if tc.refused {
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("role %q: status %d, want 401", tc.role, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "admin token") {
				t.Errorf("role %q: body %q does not name admin tokens", tc.role, rec.Body.String())
			}
			if link := rec.Header().Get("Link"); !strings.Contains(link, "admin-tokens") {
				t.Errorf("role %q: Link = %q", tc.role, link)
			}
			continue
		}
		if rec.Code == http.StatusUnauthorized && strings.Contains(rec.Body.String(), "admin role") {
			t.Errorf("role %q was refused as an admin-role key", tc.role)
		}
	}
}

func TestAdminRoleKeyDeprecation_CallersAndLogLimit(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	d := newAdminRoleKeyDeprecation(func(*http.Request) string { return "/api/admin/schemas" })
	d.now = func() time.Time { return now }
	d.logger = func() *slog.Logger { return logger }
	h := d.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	call := func(c *core.AuthClaims) int {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/schemas", nil)
		if c != nil {
			req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, c))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	key := &core.AuthClaims{UserID: "k1", APIKeyID: "k1", Roles: []string{"super_admin"}, IsAPIKey: true, TenantID: "acme"}
	session := &core.AuthClaims{UserID: "u1", Roles: []string{"super_admin"}, TenantID: "acme"}
	token := &core.AuthClaims{UserID: "u1", Roles: []string{"admin"}, AdminTokenID: "t1", TenantID: "acme"}

	if call(session) != http.StatusOK || call(token) != http.StatusOK || call(nil) != http.StatusOK {
		t.Fatal("a session, an admin token or an anonymous caller was refused")
	}

	for range 3 {
		if call(key) != http.StatusUnauthorized {
			t.Fatal("admin key not refused")
		}
	}
	if n := strings.Count(buf.String(), "api_key_id=k1"); n != 1 {
		t.Fatalf("logged %d lines within the hour, want 1:\n%s", n, buf.String())
	}
	for _, want := range []string{"tenant_id=acme", "route=/api/admin/schemas"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log line lacks %q: %s", want, buf.String())
		}
	}

	now = now.Add(adminKeyLogInterval)
	call(key)
	if n := strings.Count(buf.String(), "api_key_id=k1"); n != 2 {
		t.Fatalf("logged %d lines after the interval, want 2", n)
	}
}
