package api

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each in-process map below drops an entry only when its own key is read
// again, which for a used challenge id or an account that never returns is
// never. Past its threshold the next insert sweeps what has expired, and an
// entry still in force stays.

func TestInMemoryMFALockout_UsedJTIsArePrunedPastTheThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	expired := time.Now().Add(-2 * challengeTokenTTL)
	l.mu.Lock()
	for i := range maxTrackedJTIs {
		l.usedJTIs[fmt.Sprintf("expired-%d", i)] = expired
	}
	l.usedJTIs["live"] = time.Now()
	l.mu.Unlock()

	require.True(t, l.MarkJTIUsed("new"))

	l.mu.Lock()
	size := len(l.usedJTIs)
	l.mu.Unlock()
	assert.Equal(t, 2, size, "expired challenge ids stayed in the map")
	assert.True(t, l.IsJTIUsed("live"), "a sweep dropped an id still in force")
	assert.True(t, l.IsJTIUsed("new"))
}

func TestInMemoryMFALockout_UserWindowsArePrunedPastTheThreshold(t *testing.T) {
	t.Parallel()

	window := 15 * time.Minute
	l := NewInMemoryMFALockout(2, window)
	expired := time.Now().Add(-2 * window)
	live := uuid.New()
	l.mu.Lock()
	for range maxTrackedMFAUsers {
		state := &mfaLockoutUserState{failures: counterAt(2), createdAt: expired}
		l.entries[uuid.New()] = state
	}
	l.entries[live] = &mfaLockoutUserState{failures: counterAt(2), createdAt: time.Now()}
	l.mu.Unlock()

	_, ok := l.ReserveMFAAttempt(uuid.New())
	require.True(t, ok)

	l.mu.Lock()
	size := len(l.entries)
	l.mu.Unlock()
	assert.Equal(t, 2, size, "expired failure windows stayed in the map")
	locked, err := l.CheckMFALockout(live)
	require.NoError(t, err)
	assert.True(t, locked, "a sweep dropped a lockout still in force")
}

func TestInMemoryLockout_WindowsArePrunedPastTheThreshold(t *testing.T) {
	t.Parallel()

	window := 15 * time.Minute
	l := NewInMemoryLockout(2, window)
	expired := time.Now().Add(-2 * window).UnixNano()
	live := uuid.New()
	l.mu.Lock()
	for range maxTrackedLockouts {
		state := &lockoutUserState{}
		state.failures.Store(2)
		state.windowStart.Store(expired)
		l.entries[uuid.New()] = state
	}
	held := &lockoutUserState{}
	held.failures.Store(2)
	held.windowStart.Store(time.Now().UnixNano())
	l.entries[live] = held
	l.mu.Unlock()

	_, ok, err := l.ReserveAttempt(context.Background(), uuid.New(), 2)
	require.NoError(t, err)
	require.True(t, ok)

	l.mu.Lock()
	size := len(l.entries)
	l.mu.Unlock()
	assert.Equal(t, 2, size, "expired failure windows stayed in the map")
	_, ok, err = l.ReserveAttempt(context.Background(), live, 2)
	require.NoError(t, err)
	assert.False(t, ok, "a sweep dropped a lockout still in force")
}

func counterAt(n int64) *atomic.Int64 {
	c := new(atomic.Int64)
	c.Store(n)
	return c
}

// While every entry is in force a sweep frees nothing, and sweeping again at
// the same size would scan the whole map on every new account a credential
// stuffing run tries. The next sweep waits until the map has doubled.
func TestInMemoryLockout_SweepOfLiveWindowsWaitsForTheMapToDouble(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(2, 15*time.Minute)
	now := time.Now().UnixNano()
	l.mu.Lock()
	for range maxTrackedLockouts {
		state := &lockoutUserState{}
		state.failures.Store(1)
		state.windowStart.Store(now)
		l.entries[uuid.New()] = state
	}
	l.mu.Unlock()

	_, ok, err := l.ReserveAttempt(context.Background(), uuid.New(), 2)
	require.NoError(t, err)
	require.True(t, ok)

	l.mu.Lock()
	defer l.mu.Unlock()
	assert.Equal(t, maxTrackedLockouts+1, len(l.entries), "a sweep dropped a window still in force")
	assert.Equal(t, 2*maxTrackedLockouts, l.sweepAt)
}
