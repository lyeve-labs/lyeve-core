package core

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScopedHost_ForwardersFallBackGracefully exercises the optional-provider
// forwarders against a bare Host (stubHost implements no optional provider
// interfaces). Each must degrade to its documented safe default rather than
// panic: this is the "engine may or may not wire a given capability" contract.
func TestScopedHost_ForwardersFallBackGracefully(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "bare", CapAll)
	ctx := context.Background()

	// Unconditional pass-throughs.
	assert.NotNil(t, h.Logger(ctx))
	assert.NotNil(t, h.Tracer("bare"))

	// Absent optional providers -> safe zero/nil/no-op results.
	_, err := h.SignSessionToken(ctx, uuid.New(), "e@x.com", nil)
	assert.ErrorIs(t, err, ErrCapDenied, "no signer wired -> capability denied")

	assert.Equal(t, 0, h.InvalidateCache("prefix"))
	assert.Nil(t, h.StorageConnected())
	assert.Nil(t, h.EmailSender())
	assert.NotPanics(t, func() { h.RegisterEmailSender(nil) })
	assert.NoError(t, h.RevokeAllRefreshTokens(ctx, "user-1"))
	assert.Nil(t, h.UserProvisioner())

	// AcquireTenantConn passes the context through with a no-op release.
	gotCtx, release, err := h.AcquireTenantConn(ctx, "tenant-1")
	require.NoError(t, err)
	require.NotNil(t, release)
	assert.Equal(t, ctx, gotCtx)
	assert.NotPanics(t, release)

	// CachedFetch without a cache provider runs the fetch fn directly (miss).
	called := false
	cached, err := h.CachedFetch(ctx, "bare", "tenant-1", "postgres", "SELECT 1", nil, time.Second, nil,
		func() ([]byte, error) { called = true; return nil, nil })
	require.NoError(t, err)
	assert.False(t, cached)
	assert.True(t, called, "fetch fn must be invoked when no cache is wired")

	// A nil fetch fn is a no-op miss (no panic, no fetch).
	cached, err = h.CachedFetch(ctx, "bare", "tenant-1", "postgres", "SELECT 1", nil, time.Second, nil, nil)
	require.NoError(t, err)
	assert.False(t, cached)
}

// A host with no query cache still has to hand the fetched rows back, or a
// plugin store read through an uncached host returns an untouched destination
// and no error.
func TestScopedHost_CachedFetchFallbackFillsDest(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "bare", CapAll)

	var got map[string]int
	cached, err := h.CachedFetch(context.Background(), "bare", "tenant-1", "postgres",
		"SELECT requests_limit FROM sys_quotas", nil, time.Second, &got,
		func() ([]byte, error) { return []byte(`{"limit":11}`), nil })
	require.NoError(t, err)
	assert.False(t, cached)
	assert.Equal(t, map[string]int{"limit": 11}, got)
}
