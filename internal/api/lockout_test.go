package api

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// InMemoryLockout: unit tests

func TestInMemoryLockout_Counts_NoFailures(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "user with no failures should have count 0")
}

func TestInMemoryLockout_Counts_UnderThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 4; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	}

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 4, count, "4 failures should be reported")
}

func TestInMemoryLockout_Counts_AtThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	}

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 5, count, "5 failures should be reported")
}

func TestInMemoryLockout_Counts_AboveThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 10; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	}

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 10, count, "10 failures should be reported")
}

func TestInMemoryLockout_Counts_WindowExpiry(t *testing.T) {
	t.Parallel()

	// Use a very short window so we can test expiry.
	l := NewInMemoryLockout(5, 10*time.Millisecond)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	}

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 5, count, "should see 5 failures inside window")

	time.Sleep(20 * time.Millisecond)

	count, err = lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "should see 0 after window expired")
}

func TestInMemoryLockout_ResetFailedAttempts(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	}

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 5, count, "should see 5 failures before reset")

	l.ResetFailedAttempts(uid)

	count, err = lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "should see 0 after reset")
}

// Resetting a user that was never tracked should not panic.
func TestInMemoryLockout_ResetFailedAttempts_UnknownUser(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	assert.NotPanics(t, func() {
		l.ResetFailedAttempts(uid)
	})
}

func TestInMemoryLockout_PerUserIsolation(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid1 := uuid.New()
	uid2 := uuid.New()

	for i := 0; i < 5; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid1, noLimit))
	}
	_ = reserveErr(l.ReserveAttempt(context.Background(), uid2, noLimit))

	count1, err := lockoutCount(l, uid1)
	require.NoError(t, err)
	assert.Equal(t, 5, count1, "uid1 should have 5 failures")

	count2, err := lockoutCount(l, uid2)
	require.NoError(t, err)
	assert.Equal(t, 1, count2, "uid2 should have 1 failure - isolation broken")
}

func TestInMemoryLockout_WindowResetOnNewFailure(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 10*time.Millisecond)
	uid := uuid.New()

	_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))

	time.Sleep(20 * time.Millisecond)

	// The window has closed, so it holds nothing.
	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "after window expiry, count should be 0")

	// A new failure starts a fresh window.
	_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	count, err = lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "new failure should start a fresh window with count 1")
}

func TestNewInMemoryLockout_Defaults(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(0, 0)
	assert.Equal(t, DefaultMaxFailedAttempts, l.maxAttempts, "should use default max attempts")
	assert.Equal(t, DefaultLockoutWindow, l.window, "should use default window")
}

// An expired window holds nothing, and a new ReserveAttempt after it starts
// a fresh window at count 1.
func TestInMemoryLockout_ExpiredEntriesArePruned(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 10*time.Millisecond)
	uid := uuid.New()

	_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	time.Sleep(20 * time.Millisecond)

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// A new failure after pruning must start a fresh window at count 1.
	_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	count, err = lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "fresh window after pruning should start at count 1")
}

// ResetFailedAttempts removes the entry from the map rather than zeroing.
func TestInMemoryLockout_ResetDeletesEntry(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 3; i++ {
		_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
	}

	l.ResetFailedAttempts(uid)

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	l.mu.Lock()
	_, exists := l.entries[uid]
	l.mu.Unlock()
	assert.False(t, exists, "entry should be deleted after ResetFailedAttempts")
}

// Of many concurrent reservations exactly the limit are counted, so only
// that many attempts reach the password check.
func TestInMemoryLockout_ConcurrentReservesStopAtTheLimit(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	const callers, limit = 50, 5
	var wg sync.WaitGroup
	var granted atomic.Int32
	start := make(chan struct{})
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := l.ReserveAttempt(context.Background(), uid, limit)
			assert.NoError(t, err)
			if ok {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(limit), granted.Load())
	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, limit, count, "a refused reservation counts nothing")
}

func TestInMemoryLockout_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	const goroutines = 20
	const perGoroutine = 10
	done := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		go func() {
			for j := 0; j < perGoroutine; j++ {
				_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
			}
			done <- struct{}{}
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}

	count, err := lockoutCount(l, uid)
	require.NoError(t, err)
	assert.Equal(t, goroutines*perGoroutine, count, "concurrent increments should all be counted")
}

// The window roll is check-then-reset. Lock-free, several goroutines each read
// an expired window and each store zero over increments the others had already
// made, which loses failures during exactly the traffic this counter exists to
// measure. Repeated because the interleaving that loses them is the one at the
// very first call, when the window has never been set.
func TestInMemoryLockout_NoLostFailuresOnTheFirstBurst(t *testing.T) {
	t.Parallel()

	for round := 0; round < 200; round++ {
		l := NewInMemoryLockout(5, 15*time.Minute)
		uid := uuid.New()

		const goroutines = 20
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				<-start
				_ = reserveErr(l.ReserveAttempt(context.Background(), uid, noLimit))
			}()
		}
		close(start)
		wg.Wait()

		count, err := lockoutCount(l, uid)
		require.NoError(t, err)
		if count != goroutines {
			t.Fatalf("round %d: counted %d of %d failures", round, count, goroutines)
		}
	}
}

func TestNewAuthHandler_DefaultLockoutWired(t *testing.T) {
	h := NewAuthHandler(nil, nil, nil, "test-secret-at-least-32-bytes-long!!", 3600, false)

	assert.NotNil(t, h.lockoutChecker, "default InMemoryLockout should be wired")
	assert.NotNil(t, h.mfaLockoutChecker, "default InMemoryMFALockout should be wired")

	iml, ok := h.lockoutChecker.(*InMemoryLockout)
	require.True(t, ok, "default lockout checker should be InMemoryLockout")
	assert.Equal(t, DefaultMaxFailedAttempts, iml.maxAttempts)
	assert.Equal(t, DefaultLockoutWindow, iml.window)
}

func TestNewAuthHandler_WithLockoutCheckerOverridesDefault(t *testing.T) {
	h := NewAuthHandler(nil, nil, nil, "test-secret-at-least-32-bytes-long!!", 3600, false)

	fake := &fakeLockoutChecker{failCount: 3}
	h.WithLockoutChecker(fake)

	assert.Equal(t, LockoutChecker(fake), h.lockoutChecker, "WithLockoutChecker should override default")
}

// lockoutCount reads the attempts counted in the user's open window. Tests
// read it here because nothing in the engine reads the count back.
func lockoutCount(lc LockoutChecker, uid uuid.UUID) (int, error) {
	switch l := lc.(type) {
	case *InMemoryLockout:
		l.mu.Lock()
		defer l.mu.Unlock()
		s, ok := l.entries[uid]
		if !ok || time.Now().UnixNano()-s.windowStart.Load() > int64(l.window) {
			return 0, nil
		}
		return int(s.failures.Load()), nil
	case *SharedLockout:
		st, err := l.counter.current(context.Background(), l.key(uid), l.window, time.Now())
		return int(st.count), err
	}
	return 0, fmt.Errorf("no count to read from %T", lc)
}

// A release that brings a window to zero removes its entry, so accounts that
// sign in correctly leave nothing in the map.
func TestInMemoryLockout_ReleaseToZeroRemovesTheEntry(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()
	r, ok, err := l.ReserveAttempt(context.Background(), uid, 5)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, l.ReleaseAttempt(context.Background(), uid, r))

	l.mu.Lock()
	_, exists := l.entries[uid]
	l.mu.Unlock()
	assert.False(t, exists)
}
