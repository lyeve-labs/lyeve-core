//go:build !short && !mutest

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func openAPIPaths(t *testing.T, rr *httptest.ResponseRecorder) (admin, public int) {
	t.Helper()
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc), rr.Body.String())
	for p := range doc.Paths {
		if isAdminPath(p) {
			admin++
		} else {
			public++
		}
	}
	return admin, public
}

// The admin half is refused to an admin on the server, not hidden by the
// console, and the role-scoped document does not hand an admin the admin half.
func TestOpenAPISplit_TheAdminHalfIsForSuperAdminsOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}
	pool := testdb.Postgres(t)
	ctx := core.WithTenantID(context.Background(), "acme")
	cfg := &config.Config{
		DatabaseDriver: "postgres",
		JWTSecrets:     []string{"test-secret-at-least-32-bytes-long-ok"},
		JWTSecret:      "test-secret-at-least-32-bytes-long-ok",
		JWTExpirySecs:  3600,
		CacheTTL:       60 * time.Second,
	}
	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
	require.NoError(t, err)
	super := seedUserToken(t, ctx, pool, cfg, "super@test.com", "super_admin")
	admin := seedUserToken(t, ctx, pool, cfg, "admin@test.com", "admin")

	get := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	rr := get("/api/admin/openapi/admin.json", admin)
	assert.Equal(t, http.StatusForbidden, rr.Code)

	rr = get("/api/admin/openapi/admin.json", super)
	require.Equal(t, http.StatusOK, rr.Code)
	a, p := openAPIPaths(t, rr)
	assert.Positive(t, a)
	assert.Zero(t, p, "the admin half holds no public route")

	for _, token := range []string{admin, super} {
		rr = get("/api/admin/openapi/public.json", token)
		require.Equal(t, http.StatusOK, rr.Code)
		a, p = openAPIPaths(t, rr)
		assert.Zero(t, a, "the public half holds no admin route")
		assert.Positive(t, p)
	}

	rr = get("/api/admin/openapi.json", admin)
	require.Equal(t, http.StatusOK, rr.Code)
	a, _ = openAPIPaths(t, rr)
	assert.Zero(t, a, "the original path serves an admin the public half")

	rr = get("/api/admin/openapi.json", super)
	require.Equal(t, http.StatusOK, rr.Code)
	a, p = openAPIPaths(t, rr)
	assert.Positive(t, a, "and a super admin the whole document")
	assert.Positive(t, p)
}

func TestOpenAPIHalf_SplitsAtTheAdminPrefix(t *testing.T) {
	doc := openAPIDoc{Paths: map[string]openAPIPath{
		"/api/admin":          {},
		"/api/admin/users":    {},
		"/api/administrators": {},
		"/api/v1/{schema}":    {},
		"/hooks/stripe":       {},
	}}
	admin := openAPIHalf(doc, true)
	public := openAPIHalf(doc, false)
	assert.ElementsMatch(t, []string{"/api/admin", "/api/admin/users"}, pathKeys(admin.Paths))
	assert.ElementsMatch(t, []string{"/api/administrators", "/api/v1/{schema}", "/hooks/stripe"}, pathKeys(public.Paths))
	assert.Len(t, doc.Paths, 5, "the split does not change the document it was given")
}

func pathKeys(m map[string]openAPIPath) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// An API key carries its roles on the core claims, and the role check reads
// them the way requireRole does.
func TestCallerHasRole_ReadsSessionAndKeyClaims(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	assert.False(t, callerHasRole(r, "super_admin"))

	s := r.WithContext(context.WithValue(r.Context(), auth.ClaimsKey, &auth.Claims{Roles: []string{"super_admin"}}))
	assert.True(t, callerHasRole(s, "super_admin"))

	k := r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, &core.AuthClaims{Roles: []string{"admin"}}))
	assert.False(t, callerHasRole(k, "super_admin"))
	assert.True(t, callerHasRole(k, "admin"))
}
