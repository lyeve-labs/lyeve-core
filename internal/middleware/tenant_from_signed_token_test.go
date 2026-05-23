package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The whole chain, from the token the login handler signs to the value a
// plugin store reads: a tenant-scoped user with no header and no domain
// mapping must still land on their own tenant. Both context keys are checked
// because plugins are required to use core.TenantIDFromCtx while the engine's
// own tenancy uses the middleware key, and a value on only one of them is a
// silent isolation gap.
func TestTenantHeader_ResolvesTenantFromASignedToken(t *testing.T) {
	const secret = "tenant-from-token-secret-32-bytes!"

	token, err := auth.Sign(secret, 3600, uuid.New(), "member@acme.test", []string{"admin"}, "acme", 1)
	require.NoError(t, err)
	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)

	var fromMiddleware, fromCore string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fromMiddleware = mw.TenantIDFromCtx(r)
		fromCore = core.TenantIDFromCtx(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/schemas", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	mw.TenantHeader(true)(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "acme", fromMiddleware, "engine tenancy must see the tenant")
	assert.Equal(t, "acme", fromCore, "plugins must see the same tenant")
}

// A tenant user cannot reach another tenant by asking for it. The override is
// super_admin only, so the header is ignored and the JWT wins.
func TestTenantHeader_TenantUserCannotOverrideWithAHeader(t *testing.T) {
	const secret = "tenant-no-override-secret-32bytes!"

	token, err := auth.Sign(secret, 3600, uuid.New(), "member@acme.test", []string{"admin"}, "acme", 1)
	require.NoError(t, err)
	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)

	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = core.TenantIDFromCtx(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/schemas", nil)
	req.Header.Set("X-Tenant-ID", "rival")
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	mw.TenantHeader(true)(next).ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "acme", seen)
}
