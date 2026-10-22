package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pluginCASBackend reports a lost compare-and-set the way a cache plugin's
// backend does, with an error of its own rather than ErrCASFailed, which a
// plugin cannot import. beforeCAS runs once ahead of the comparison, so a
// test can let another replica rotate in between.
type pluginCASBackend struct {
	failingBackend
	mismatch  func(key string) error
	beforeCAS func()
}

func (b *pluginCASBackend) CompareAndSet(ctx context.Context, key string, oldVal, newVal []byte, ttl time.Duration) error {
	if run := b.beforeCAS; run != nil {
		b.beforeCAS = nil
		run()
	}
	if b.broken {
		return errBackendDown
	}
	cur, _ := b.inner.Get(ctx, key)
	if !bytes.Equal(cur, oldVal) {
		return b.mismatch(key)
	}
	return b.inner.Set(ctx, key, newVal, ttl)
}

// A refresh that loses the race to another replica answers ErrTokenRotated
// whichever backend holds the families, never a generic failure the handler
// reads as a bad token.
func TestRotate_PluginBackendLostRaceIsRotated(t *testing.T) {
	sentinel := errors.New("cacheprovider: CAS mismatch: value changed")
	for name, mismatch := range map[string]func(string) error{
		"redis sentinel":   func(string) error { return sentinel },
		"memory string":    func(key string) error { return fmt.Errorf("cache: CAS mismatch: key %q", key) },
		"wrapped sentinel": func(string) error { return fmt.Errorf("cas: %w", sentinel) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			inner := NewMemoryBackend()
			pb := &pluginCASBackend{failingBackend: failingBackend{inner: inner}, mismatch: mismatch}
			loser := NewRefreshTokenStore(pb, "test")
			winner := NewRefreshTokenStore(inner, "test")

			issued, err := loser.Issue(ctx, "user-1", 5*time.Minute)
			require.NoError(t, err)
			var won *RotateResult
			pb.beforeCAS = func() {
				won, err = winner.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
				require.NoError(t, err)
			}

			_, err = loser.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
			require.ErrorIs(t, err, ErrTokenRotated, "the losing trade of a race")
			_, err = winner.Rotate(ctx, won.RefreshToken, 5*time.Minute)
			require.NoError(t, err, "the race must not end the family")
		})
	}
}

// A compare-and-set that fails because the backend cannot be reached is an
// outage, never a lost race and never a bad token.
func TestRotate_PluginBackendUnreachableAtTheWriteIsAnOutage(t *testing.T) {
	ctx := context.Background()
	pb := &pluginCASBackend{failingBackend: failingBackend{inner: NewMemoryBackend()}}
	store := NewRefreshTokenStore(pb, "test")
	issued, err := store.Issue(ctx, "user-1", 5*time.Minute)
	require.NoError(t, err)
	pb.beforeCAS = func() { pb.broken = true }

	_, err = store.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.ErrorIs(t, err, ErrBackendUnavailable)
	require.NotErrorIs(t, err, ErrCASFailed)
}
