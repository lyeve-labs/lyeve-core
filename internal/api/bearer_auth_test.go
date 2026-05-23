//go:build !mutest

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// requireRole guards both routers. An admin UI may forward the session as a
// bearer token, so a matching role passes whatever the credential shape.
func TestRequireRole_AcceptsBearerOriginatedClaims(t *testing.T) {
	claims := &auth.Claims{UserID: "u1", Roles: []string{"admin"}}

	tests := []struct {
		name   string
		source authSourceValue
	}{
		{"bearer header", authSourceBearer},
		{"session cookie", authSourceCookie},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reached := false
			h := requireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/content/article", nil)
			ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
			ctx = context.WithValue(ctx, core.ClaimsKey, claims.AuthClaims())
			ctx = context.WithValue(ctx, authSourceCtxKey{}, tt.source)
			req = req.WithContext(ctx)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if !reached {
				t.Errorf("%s was rejected with %d; the role matched, so it must pass", tt.name, rec.Code)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		})
	}
}

// Accepting either credential shape must not weaken role enforcement.
func TestRequireRole_StillRejectsAMissingRole(t *testing.T) {
	claims := &auth.Claims{UserID: "u1", Roles: []string{"viewer"}}
	h := requireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run for a caller lacking the role")
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/content/article", nil)
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	ctx = context.WithValue(ctx, authSourceCtxKey{}, authSourceBearer)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// A key with scope *:* and no roles must not satisfy a role gate. Scope is an
// additional check, never a role.
func TestRequireRole_RolelessAPIKeyIsNotSuperAdmin(t *testing.T) {
	key := &core.AuthClaims{
		UserID:   "k1",
		Roles:    nil,
		Scopes:   []string{"*:*"},
		IsAPIKey: true,
		APIKeyID: "ak_1",
	}

	for _, role := range []string{"super_admin", "admin"} {
		t.Run(role, func(t *testing.T) {
			h := requireRole(role)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("a key holding no roles must not satisfy requireRole(%q)", role)
			}))

			// Only core.ClaimsKey: the API-key middleware sets that one, and
			// claimsFromCtx reads auth.ClaimsKey, so the JWT branch is skipped.
			req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas/migrate", nil)
			req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, key))

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

// A key holding the role passes, so the gate refuses a role-less key rather
// than every API key.
func TestRequireRole_APIKeyWithTheRolePasses(t *testing.T) {
	key := &core.AuthClaims{UserID: "k2", Roles: []string{"super_admin"}, IsAPIKey: true}

	reached := false
	h := requireRole("super_admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas/migrate", nil)
	req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, key))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached || rec.Code != http.StatusOK {
		t.Errorf("a key holding super_admin must pass; status = %d", rec.Code)
	}
}
