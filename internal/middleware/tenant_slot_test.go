package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tenancy is resolved partway down the chain, onto a context only the handlers
// below it ever see. A middleware that wrapped the whole chain still holds the
// request it was given, so anything it records afterwards has no tenant.
//
// The outer middleware installs a slot before the chain runs, TenantHeader
// fills it, and the outer middleware reads it back.
func TestTenantHeader_ReportsTheResolvedTenantToAnOuterMiddleware(t *testing.T) {
	var seen string

	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, slot := core.WithTenantSlot(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
			seen = slot.Get()
		})
	}

	claims := &auth.Claims{UserID: "u1", Roles: []string{"admin"}, TenantID: "acme"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	handler := outer(mw.TenantHeader(true)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	)))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "acme", seen,
		"the frame that wrapped the chain must be able to name the tenant it ran as")
}

// A request that resolves no tenant leaves the slot empty rather than guessing.
func TestTenantHeader_AnUnresolvedTenantLeavesTheSlotEmpty(t *testing.T) {
	var seen = "unset"

	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, slot := core.WithTenantSlot(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
			seen = slot.Get()
		})
	}

	handler := outer(mw.TenantHeader(true)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	)))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Empty(t, seen)
}

// TenantHeader must not require a slot: it is installed by one middleware and
// every other caller of TenantHeader has none.
func TestTenantHeader_WorksWithNoSlotInstalled(t *testing.T) {
	claims := &auth.Claims{UserID: "u1", TenantID: "acme"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	var tenant string
	rec := httptest.NewRecorder()
	mw.TenantHeader(true)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		tenant = mw.TenantIDFromCtx(r)
	})).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "acme", tenant)
}

func TestTenantSlot_NilIsSafe(t *testing.T) {
	var s *core.TenantSlot
	assert.NotPanics(t, func() { s.Set("acme") })
	assert.Empty(t, s.Get())
	assert.Nil(t, core.TenantSlotFrom(context.Background()))
}
