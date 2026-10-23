package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Default constants

// DefaultMFAVerifyMaxAttempts is the default per-user MFA failure cap before
// lockout. Set to 5. Can be overridden via NewInMemoryMFALockout.
const DefaultMFAVerifyMaxAttempts = 5

// DefaultMFALockoutWindow is the sliding window within which failures are
// counted. Default 15 minutes.
const DefaultMFALockoutWindow = 15 * time.Minute

// DefaultMFAChallengeMaxAttempts is the default challenge-level attempt cap.
// Each challenge token can be attempted this many times before it is exhausted.
const DefaultMFAChallengeMaxAttempts = 5

// maxTrackedChallenges is the soft cap on the challenge-attempt map. When a new
// entry would push the map past this size, expired entries are swept first.
// Unique challenge hashes are never re-accessed once their flow ends, so the
// per-key lazy prune cannot reclaim them on its own.
const maxTrackedChallenges = 4096

// Types

// InMemoryMFALockout provides per-user and per-challenge MFA attempt tracking.
// It is wired by default in AuthHandler so MFAVerify enforces a lockout
// whether or not a plugin supplies one.
//
// In addition to brute-force protection (per-user lockout and per-challenge
// attempt limits), InMemoryMFALockout tracks used challenge JTIs for single-use
// enforcement: a successfully verified challenge token cannot be replayed
// within its 5-minute window.
type InMemoryMFALockout struct {
	mu sync.Mutex

	maxAttempts          int
	window               time.Duration
	maxChallengeAttempts int

	entries          map[uuid.UUID]*mfaLockoutUserState
	challengeEntries map[string]*challengeEntryState

	// usedJTIs tracks challenge token JTIs that have been successfully
	// verified. Entries are pruned after the challenge token TTL (5 min).
	// This prevents replay of a valid challenge within its expiry window.
	usedJTIs map[string]time.Time
}

// mfaLockoutUserState tracks failures and the last reset time for a single user.
type mfaLockoutUserState struct {
	failures  *atomic.Int64
	createdAt time.Time
}

// challengeEntryState tracks per-challenge attempts with an expiry timestamp
// so stale entries can be pruned. Without pruning, an attacker sending many
// unique challenge tokens causes unbounded memory growth.
type challengeEntryState struct {
	counter   *atomic.Int64
	createdAt time.Time
}

// NewInMemoryMFALockout creates an InMemoryMFALockout with the given limits.
// When maxAttempts ≤ 0, DefaultMFAVerifyMaxAttempts is used.
// When window ≤ 0, DefaultMFALockoutWindow is used.
func NewInMemoryMFALockout(maxAttempts int, window time.Duration) *InMemoryMFALockout {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMFAVerifyMaxAttempts
	}
	if window <= 0 {
		window = DefaultMFALockoutWindow
	}
	return &InMemoryMFALockout{
		maxAttempts:          maxAttempts,
		window:               window,
		maxChallengeAttempts: DefaultMFAChallengeMaxAttempts,
		entries:              make(map[uuid.UUID]*mfaLockoutUserState),
		challengeEntries:     make(map[string]*challengeEntryState),
		usedJTIs:             make(map[string]time.Time),
	}
}

// CheckMFALockout returns true when the user has exceeded the failure limit
// within the sliding window. Failures outside the window are pruned.
func (l *InMemoryMFALockout) CheckMFALockout(userID uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.entries[userID]
	if !ok {
		return false, nil
	}

	// Prune if window has expired.
	if time.Since(state.createdAt) > l.window {
		delete(l.entries, userID)
		return false, nil
	}

	return state.failures.Load() >= int64(l.maxAttempts), nil
}

// ReserveMFAAttempt counts one code attempt against the user before the code
// is checked, and returns false without counting once the user has reached
// the failure limit. The read and the increment are one step under l.mu, so
// of many concurrent attempts only the limit reach the code check. A window
// that has expired starts again.
func (l *InMemoryMFALockout) ReserveMFAAttempt(userID uuid.UUID) (Reservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.entries[userID]
	if !ok || time.Since(state.createdAt) > l.window {
		state = &mfaLockoutUserState{
			failures:  new(atomic.Int64),
			createdAt: time.Now(),
		}
		l.entries[userID] = state
	}
	r := Reservation{Count: int(state.failures.Load()), window: state.createdAt.UnixNano()}
	if r.Count >= l.maxAttempts {
		return r, false
	}
	r.Count = int(state.failures.Add(1))
	return r, true
}

// ReleaseMFAAttempt gives back one attempt reserved in the user's current
// window. A window that has expired or been replaced is left as it is, and a
// window whose count reaches zero is removed.
func (l *InMemoryMFALockout) ReleaseMFAAttempt(userID uuid.UUID, r Reservation) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.entries[userID]
	if !ok || state.createdAt.UnixNano() != r.window || time.Since(state.createdAt) > l.window {
		return
	}
	if state.failures.Load() <= 1 {
		delete(l.entries, userID)
		return
	}
	state.failures.Add(-1)
}

// ResetMFAAttempts clears the failure counter for the given user.
func (l *InMemoryMFALockout) ResetMFAAttempts(userID uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, userID)
}

// ReserveChallengeAttempt spends one of the challenge's attempts, and returns
// false without spending once they are used up. The counter is raised by
// compare-and-swap from a value under the cap, so concurrent attempts on one
// challenge cannot pass the cap together.
func (l *InMemoryMFALockout) ReserveChallengeAttempt(challengeHash string) bool {
	counter := l.loadChallengeCounter(challengeHash)
	for {
		n := counter.Load()
		if n >= int64(l.maxChallengeAttempts) {
			return false
		}
		if counter.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// loadChallengeCounter returns the atomic counter for a challenge hash,
// creating a fresh entry when none exists or the old one has expired.
// Expired entries (older than challengeTokenTTL) are pruned on access.
func (l *InMemoryMFALockout) loadChallengeCounter(challengeHash string) *atomic.Int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.challengeEntries[challengeHash]
	if ok {
		// Prune expired entries: challenge tokens have a 5-minute TTL.
		if time.Since(entry.createdAt) > challengeTokenTTL {
			delete(l.challengeEntries, challengeHash)
			ok = false
		}
	}

	if !ok {
		// keep the map bounded (see maxTrackedChallenges)
		if len(l.challengeEntries) >= maxTrackedChallenges {
			l.pruneExpiredChallengesLocked()
		}
		entry = &challengeEntryState{
			counter:   new(atomic.Int64),
			createdAt: time.Now(),
		}
		l.challengeEntries[challengeHash] = entry
	}

	return entry.counter
}

// pruneExpiredChallengesLocked deletes challenge entries older than the
// challenge-token TTL. The caller must hold l.mu.
func (l *InMemoryMFALockout) pruneExpiredChallengesLocked() {
	for hash, entry := range l.challengeEntries {
		if time.Since(entry.createdAt) > challengeTokenTTL {
			delete(l.challengeEntries, hash)
		}
	}
}

// challengeTokenTTL matches the 5-minute expiry set in auth.SignChallenge.
const challengeTokenTTL = 5 * time.Minute

// MarkJTIUsed records a challenge token JTI as consumed and reports whether
// this call recorded it. Of two calls for one JTI only the first gets true.
// After this call, IsJTIUsed returns true for the same JTI until the entry
// expires.
func (l *InMemoryMFALockout) MarkJTIUsed(jti string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seen, ok := l.usedJTIs[jti]; ok && time.Since(seen) <= challengeTokenTTL {
		return false
	}
	l.usedJTIs[jti] = time.Now()
	return true
}

// IsJTIUsed returns true when the given JTI has already been consumed
// and the entry hasn't expired yet. Entries older than challengeTokenTTL
// are pruned on read.
func (l *InMemoryMFALockout) IsJTIUsed(jti string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	seen, ok := l.usedJTIs[jti]
	if !ok {
		return false
	}
	if time.Since(seen) > challengeTokenTTL {
		delete(l.usedJTIs, jti)
		return false
	}
	return true
}

// Helpers

// sha256Hex returns the lowercase hex-encoded SHA-256 digest of s.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
