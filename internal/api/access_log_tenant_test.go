package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// accessLogged sends one request through the access log and the tenant
// middleware, with the credential a session or an API key leaves on the
// context, and answers 200.
func accessLogged(t *testing.T, path string, session *auth.Claims, key *core.AuthClaims, header map[string]string) {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if session != nil {
			ctx = context.WithValue(ctx, auth.ClaimsKey, session)
			ctx = context.WithValue(ctx, core.ClaimsKey, session.AuthClaims())
		}
		if key != nil {
			ctx = context.WithValue(ctx, core.ClaimsKey, key)
		}
		apimw.TenantHeader(true)(ok).ServeHTTP(w, r.WithContext(ctx))
	})
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	structuredLogger(authed).ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, path)
}

// A super admin acting in acme, by home tenant, by X-Tenant-ID or by an API
// key, requests routes whose path names globex. acme's admin has raised
// acme's level to Debug. Each access line keeps its raw path and goes under
// no tenant, so nothing acme's admins read names globex. An acme admin's own
// request keeps acme's stamp, so a tenant-scoped search still finds it.
func TestAccessLog_SuperAdminRequestCarriesNoTenant(t *testing.T) {
	const home, other = "acme", "globex"
	ring, sink := engineLogs(t, home)

	paths := []string{
		"/api/admin/quotas/" + other,
		"/api/admin/usage/tenant/" + other,
		"/api/admin/tenant-features/" + other,
		"/api/admin/logging/levels/tenant/" + other,
	}
	homeSuper := &auth.Claims{UserID: uuid.NewString(), Roles: []string{"super_admin"}, TenantID: home}
	scopedSuper := &auth.Claims{UserID: uuid.NewString(), Roles: []string{"super_admin"}, TenantID: core.DefaultTenantSlug}
	keySuper := &core.AuthClaims{UserID: uuid.NewString(), Roles: []string{"super_admin"}, TenantID: home}
	for _, p := range paths {
		accessLogged(t, p, homeSuper, nil, nil)
		accessLogged(t, p, scopedSuper, nil, map[string]string{"X-Tenant-ID": home})
		accessLogged(t, p, nil, keySuper, nil)
	}
	assert.Equal(t, 3*len(paths), countNaming(ring, other), "every access line is still logged with its path")
	for _, line := range visibleTo(t, home, ring, sink) {
		assert.NotContains(t, line, other, "a record acme's admins read names another tenant")
	}

	admin := &auth.Claims{UserID: uuid.NewString(), Roles: []string{"admin"}, TenantID: home}
	accessLogged(t, "/api/admin/schemas/posts", admin, nil, nil)
	own := 0
	for _, line := range visibleTo(t, home, ring, sink) {
		if assert.Contains(t, line, "/api/admin/schemas/posts") {
			own++
		}
	}
	assert.Equal(t, 2, own, "an admin's access line is in acme's ring and stored log")
}
