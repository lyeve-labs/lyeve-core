package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// InMemoryLockout: unit tests

func TestInMemoryLockout_CountRecentFailures_NoFailures(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, count, "user with no failures should have count 0")
}

func TestInMemoryLockout_CountRecentFailures_UnderThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 4; i++ {
		_ = l.RecordFailedAttempt(context.Background(), uid)
	}

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 4, count, "4 failures should be reported")
}

func TestInMemoryLockout_CountRecentFailures_AtThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		_ = l.RecordFailedAttempt(context.Background(), uid)
	}

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 5, count, "5 failures should be reported")
}

func TestInMemoryLockout_CountRecentFailures_AboveThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 10; i++ {
		_ = l.RecordFailedAttempt(context.Background(), uid)
	}

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 10, count, "10 failures should be reported")
}

func TestInMemoryLockout_CountRecentFailures_WindowExpiry(t *testing.T) {
	t.Parallel()

	// Use a very short window so we can test expiry.
	l := NewInMemoryLockout(5, 10*time.Millisecond)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		_ = l.RecordFailedAttempt(context.Background(), uid)
	}

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 5, count, "should see 5 failures inside window")

	time.Sleep(20 * time.Millisecond)

	count, err = l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 0, count, "should see 0 after window expired")
}

func TestInMemoryLockout_ResetFailedAttempts(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		_ = l.RecordFailedAttempt(context.Background(), uid)
	}

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 5, count, "should see 5 failures before reset")

	l.ResetFailedAttempts(uid)

	count, err = l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
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
		_ = l.RecordFailedAttempt(context.Background(), uid1)
	}
	_ = l.RecordFailedAttempt(context.Background(), uid2)

	count1, err := l.CountRecentFailures(context.Background(), uid1, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 5, count1, "uid1 should have 5 failures")

	count2, err := l.CountRecentFailures(context.Background(), uid2, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, count2, "uid2 should have 1 failure - isolation broken")
}

func TestInMemoryLockout_WindowResetOnNewFailure(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 10*time.Millisecond)
	uid := uuid.New()

	_ = l.RecordFailedAttempt(context.Background(), uid)

	time.Sleep(20 * time.Millisecond)

	// CountRecentFailures with a since inside the current moment should see 0.
	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 0, count, "after window expiry, count should be 0")

	// A new failure starts a fresh window.
	_ = l.RecordFailedAttempt(context.Background(), uid)
	count, err = l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 1, count, "new failure should start a fresh window with count 1")
}

func TestNewInMemoryLockout_Defaults(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(0, 0)
	assert.Equal(t, DefaultMaxFailedAttempts, l.maxAttempts, "should use default max attempts")
	assert.Equal(t, DefaultLockoutWindow, l.window, "should use default window")
}

// Expired entries are deleted from the underlying map on CountRecentFailures
// (unbounded map growth). A new RecordFailedAttempt after pruning
// starts a fresh window at count 1.
func TestInMemoryLockout_ExpiredEntriesArePruned(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 10*time.Millisecond)
	uid := uuid.New()

	_ = l.RecordFailedAttempt(context.Background(), uid)
	time.Sleep(20 * time.Millisecond)

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// A new failure after pruning must start a fresh window at count 1.
	_ = l.RecordFailedAttempt(context.Background(), uid)
	count, err = l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, 1, count, "fresh window after pruning should start at count 1")
}

// Recording failures for many distinct users and then letting the window
// expire must result in a clean map: entries are pruned, preventing
// unbounded growth.
func TestInMemoryLockout_ManyUsersPrunedAfterExpiry(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 10*time.Millisecond)
	userCount := 50
	userIDs := make([]uuid.UUID, userCount)

	for i := 0; i < userCount; i++ {
		userIDs[i] = uuid.New()
		_ = l.RecordFailedAttempt(context.Background(), userIDs[i])
	}

	time.Sleep(20 * time.Millisecond)

	for _, uid := range userIDs {
		count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-5*time.Millisecond))
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	}

	l.mu.Lock()
	remaining := len(l.entries)
	l.mu.Unlock()
	assert.Equal(t, 0, remaining, "all expired entries should be pruned, found %d", remaining)
}

// ResetFailedAttempts removes the entry from the map rather than zeroing.
func TestInMemoryLockout_ResetDeletesEntry(t *testing.T) {
	t.Parallel()

	l := NewInMemoryLockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 3; i++ {
		_ = l.RecordFailedAttempt(context.Background(), uid)
	}

	l.ResetFailedAttempts(uid)

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	l.mu.Lock()
	_, exists := l.entries[uid]
	l.mu.Unlock()
	assert.False(t, exists, "entry should be deleted after ResetFailedAttempts")
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
				_ = l.RecordFailedAttempt(context.Background(), uid)
			}
			done <- struct{}{}
		}()
	}

	for i := 0; i < goroutines; i++ {
		<-done
	}

	count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
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
				_ = l.RecordFailedAttempt(context.Background(), uid)
			}()
		}
		close(start)
		wg.Wait()

		count, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-15*time.Minute))
		require.NoError(t, err)
		if count != goroutines {
			t.Fatalf("round %d: counted %d of %d failures", round, count, goroutines)
		}
	}
}

// A login counts before it records, so the two race on every attempt. Counting
// prunes an entry whose window has not been stamped (a fresh one reads as
// expired), and a record holding only a pointer to it would then increment a
// state no longer in the map. This interleaves the pair the way a login does.
func TestInMemoryLockout_CountDoesNotEatAConcurrentFailure(t *testing.T) {
	t.Parallel()

	for round := 0; round < 200; round++ {
		l := NewInMemoryLockout(5, 15*time.Minute)
		uid := uuid.New()
		since := time.Now().Add(-15 * time.Minute)

		const attempts = 20
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(attempts)
		for i := 0; i < attempts; i++ {
			go func() {
				defer wg.Done()
				<-start
				// The order a login uses.
				_, _ = l.CountRecentFailures(context.Background(), uid, since)
				_ = l.RecordFailedAttempt(context.Background(), uid)
			}()
		}
		close(start)
		wg.Wait()

		count, err := l.CountRecentFailures(context.Background(), uid, since)
		require.NoError(t, err)
		if count != attempts {
			t.Fatalf("round %d: counted %d of %d failures", round, count, attempts)
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
