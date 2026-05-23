package auth_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Flush on the wrapper must not delete the underlying data.
func TestFlushProtectedBackend_FlushIsNoOp(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()

	key := "cms:rt:family:abc123"
	require.NoError(t, real.Set(ctx, key, []byte(`{"user_id":"u1"}`), time.Minute))

	protected := auth.NewFlushProtectedBackend(real)

	require.NoError(t, protected.Flush(ctx))

	val, err := real.Get(ctx, key)
	require.NoError(t, err)
	assert.NotEmpty(t, val, "flush-protected backend should not delete data")
}

func TestFlushProtectedBackend_FlushProtectsRefreshTokenStore(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	store := auth.NewRefreshTokenStore(protected, "lyeve")

	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)

	rotated, err := store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)

	// A provider flushes the shared backend.
	require.NoError(t, protected.Flush(ctx))

	_, err = store.Rotate(ctx, rotated.RefreshToken, 5*time.Minute)
	require.NoError(t, err, "rotation after flush should succeed - tokens must survive cache flush")
}

func TestFlushProtectedBackend_GetSetDelete(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	require.NoError(t, protected.Set(ctx, "k1", []byte("v1"), time.Minute))

	val, err := protected.Get(ctx, "k1")
	require.NoError(t, err)
	assert.Equal(t, []byte("v1"), val)

	require.NoError(t, protected.Delete(ctx, "k1"))
	_, err = protected.Get(ctx, "k1")
	assert.Error(t, err, "deleted key should not be found")
}

// CompareAndSet reaches the wrapped backend, which is what protects a
// rotation racing on another replica.
func TestFlushProtectedBackend_CASDelegate(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	require.NoError(t, protected.Set(ctx, "cas-key", []byte("old"), time.Minute))

	err := protected.CompareAndSet(ctx, "cas-key", []byte("old"), []byte("new"), time.Minute)
	require.NoError(t, err)

	val, err := protected.Get(ctx, "cas-key")
	require.NoError(t, err)
	assert.Equal(t, []byte("new"), val)

	err = protected.CompareAndSet(ctx, "cas-key", []byte("wrong"), []byte("newer"), time.Minute)
	assert.Error(t, err, "CAS with wrong oldVal should fail")
}

func TestFlushProtectedBackend_PingDelegate(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	// Ping should succeed (MemoryBackend always returns nil from Ping).
	err := protected.Ping(ctx)
	assert.NoError(t, err)
}

func TestFlushProtectedBackend_AcquireLockDelegate(t *testing.T) {
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	// MemoryBackend doesn't implement AcquireLock: should return no-op release.
	release, err := protected.AcquireLock(context.Background(), "lock-key", 5*time.Second)
	assert.NoError(t, err)
	assert.NotNil(t, release)
	assert.NoError(t, release())
}

func TestFlushProtectedBackend_InterfaceSatisfaction(t *testing.T) {
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	var _ core.CacheBackend = protected
	var _ auth.CASBackend = protected

	// The Rotate path checks for AcquireLock via type assertion.
	_, ok := interface{}(protected).(interface {
		AcquireLock(ctx context.Context, key string, ttl time.Duration) (func() error, error)
	})
	assert.True(t, ok, "wrapper should support AcquireLock for cross-replica locking")
}

func TestFlushProtectedBackend_BackendUnwrap(t *testing.T) {
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	assert.Equal(t, real, protected.Backend(), "Backend() should return the real backend")
}

func TestFlushProtectedBackend_ConcurrentSafety(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "concurrent-key"
			_ = protected.Set(ctx, key, []byte("value"), time.Minute)
			_, _ = protected.Get(ctx, key)
			_ = protected.Flush(ctx) // no-op, but exercises the path under concurrency
		}(i)
	}
	wg.Wait()
	// No race detector failure = pass.
}

// A refresh token keeps rotating across a cache flush: issue, rotate, flush,
// then rotate again.
func TestFlushProtectedBackend_RefreshTokenRotationEndToEnd(t *testing.T) {
	ctx := context.Background()
	real := auth.NewMemoryBackend()
	protected := auth.NewFlushProtectedBackend(real)

	store := auth.NewRefreshTokenStore(protected, "lyeve")

	u1, err := store.Issue(ctx, "user-1", 10*time.Minute)
	require.NoError(t, err)
	u2, err := store.Issue(ctx, "user-2", 10*time.Minute)
	require.NoError(t, err)

	u1r, err := store.Rotate(ctx, u1.RefreshToken, 10*time.Minute)
	require.NoError(t, err)
	u2r, err := store.Rotate(ctx, u2.RefreshToken, 10*time.Minute)
	require.NoError(t, err)

	require.NoError(t, protected.Flush(ctx))

	_, err = store.Rotate(ctx, u1r.RefreshToken, 10*time.Minute)
	assert.NoError(t, err, "user-1 must survive cache flush")

	_, err = store.Rotate(ctx, u2r.RefreshToken, 10*time.Minute)
	assert.NoError(t, err, "user-2 must survive cache flush")
}

type countingBackend struct {
	auth.MemoryBackend
	flushCount int
}

func (b *countingBackend) Flush(ctx context.Context) error {
	b.flushCount++
	return b.MemoryBackend.Flush(ctx)
}

func TestFlushProtectedBackend_RealFlushNotCalled(t *testing.T) {
	ctx := context.Background()
	real := &countingBackend{MemoryBackend: *auth.NewMemoryBackend()}
	protected := auth.NewFlushProtectedBackend(real)

	require.NoError(t, protected.Set(ctx, "k", []byte("v"), time.Minute))
	require.NoError(t, protected.Flush(ctx))

	assert.Equal(t, 0, real.flushCount, "real backend Flush should never be called through wrapper")
}

// A dedicated auth backend, kept apart from the cache manager and wrapped in
// FlushProtectedBackend, as the runtime builds it when a provider implements
// core.AuthBackendProvider.
func TestFlushProtectedBackend_AuthBackendIsolation(t *testing.T) {
	ctx := context.Background()

	// A separate cache instance, not registered with the cache manager, stands
	// in for what a provider's AuthBackend() returns.
	authBackend := auth.NewMemoryBackend()

	protected := auth.NewFlushProtectedBackend(authBackend)

	store := auth.NewRefreshTokenStore(protected, "lyeve")

	issued, err := store.Issue(ctx, "user-1", 10*time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, issued.RefreshToken)

	rotated, err := store.Rotate(ctx, issued.RefreshToken, 10*time.Minute)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.NoError(t, protected.Flush(ctx), "flush %d should succeed (no-op)", i+1)
	}

	_, err = store.Rotate(ctx, rotated.RefreshToken, 10*time.Minute)
	require.NoError(t, err, "rotation after multiple flushes must succeed")

	_, err = authBackend.Get(ctx, "lyeve:rt:family:"+rotated.FamilyID)
	require.NoError(t, err, "family record must survive all flushes")
}
