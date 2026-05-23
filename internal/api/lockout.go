package api

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// DefaultMaxFailedAttempts is the default max failed login attempts
// per user within the lockout window before the account is locked out.
const DefaultMaxFailedAttempts = 5

// DefaultLockoutWindow is the default window for counting login failures.
const DefaultLockoutWindow = 15 * time.Minute

// lockoutUserState is one user's failure window.
//
// Both fields are read and written only under the owning InMemoryLockout's
// mutex. The entry's contents and its lifetime need the same lock: counting
// prunes an expired entry from the map, so a record holding only a pointer to
// it could stamp its failure onto a state nobody will read again.
type lockoutUserState struct {
	failures    atomic.Int64
	windowStart atomic.Int64 // unix nanos of the first failure in the current window
}

// InMemoryLockout is a process-local, per-user login failure tracker.
// It enforces a sliding-window limit: after maxAttempts failures within
// the configured window, CountRecentFailures returns the failure count.
//
// Unlike InMemoryMFALockout, this does not track per-challenge attempts
// (MFA is a separate concern). It only tracks password-login failures.
//
// Safe for concurrent use. Zero value is NOT usable: use NewInMemoryLockout.
type InMemoryLockout struct {
	maxAttempts int
	window      time.Duration

	mu      sync.Mutex
	entries map[uuid.UUID]*lockoutUserState
}

// NewInMemoryLockout creates an in-memory lockout checker for password
// login brute-force protection. Zero-value config fields fall back to
// package defaults.
func NewInMemoryLockout(maxAttempts int, window time.Duration) *InMemoryLockout {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxFailedAttempts
	}
	if window <= 0 {
		window = DefaultLockoutWindow
	}
	return &InMemoryLockout{
		maxAttempts: maxAttempts,
		window:      window,
		entries:     make(map[uuid.UUID]*lockoutUserState),
	}
}

func (l *InMemoryLockout) getOrCreate(userID uuid.UUID) *lockoutUserState {
	s, ok := l.entries[userID]
	if !ok {
		s = &lockoutUserState{}
		l.entries[userID] = s
	}
	return s
}

// CountRecentFailures returns the number of failed login attempts for the
// given user since the supplied time. If no active failure window exists
// or the window started before `since`, returns 0.
//
// Expired entries are pruned from the underlying map to prevent unbounded
// growth from non-existent or attacker-induced user IDs.
func (l *InMemoryLockout) CountRecentFailures(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	l.mu.Lock()
	s, ok := l.entries[userID]
	if !ok {
		l.mu.Unlock()
		return 0, nil
	}

	windowStart := s.windowStart.Load()

	// Window expired or started before the cutoff: prune and return 0.
	if time.Unix(0, windowStart).Before(since) {
		delete(l.entries, userID)
		l.mu.Unlock()
		return 0, nil
	}
	n := int(s.failures.Load())
	l.mu.Unlock()

	return n, nil
}

// RecordFailedAttempt records a failed login attempt for the user. If no
// active window exists (or the previous window expired), a new window
// starts now.
// The whole operation runs under l.mu, including the roll. Releasing the lock
// after getOrCreate and finishing on the entry alone is not enough: a
// concurrent CountRecentFailures prunes an entry whose window has not been
// stamped yet (a fresh one has windowStart zero, which is before any cutoff),
// and this then increments a state that is no longer in the map. The failure is
// lost, and a login calls Count immediately before Record, so the two race on
// every attempt.
func (l *InMemoryLockout) RecordFailedAttempt(ctx context.Context, userID uuid.UUID) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	s := l.getOrCreate(userID)
	now := time.Now().UnixNano()

	// Start a fresh window if the previous one expired. A zero windowStart is
	// a state that has never recorded anything, which counts as expired.
	if now-s.windowStart.Load() > int64(l.window) {
		s.failures.Store(0)
		s.windowStart.Store(now)
	}

	s.failures.Add(1)
	return nil
}

// ResetFailedAttempts clears the failure counter for the given user and
// removes the entry from the map to reclaim memory.
func (l *InMemoryLockout) ResetFailedAttempts(userID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, userID)
}
