package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// sharedLockoutKeyspace prefixes every key the shared lockouts write. The
// backend is the one the refresh-token store uses, so the prefix keeps the two
// from ever reading each other's records.
const sharedLockoutKeyspace = "lockout:"

// maxCounterCASAttempts bounds the compare-and-set retry loop. Each retry
// means another replica wrote the same counter between our read and our
// write, so a handful is plenty. Running out is reported as an error rather
// than dropping the failure silently.
const maxCounterCASAttempts = 16

// errCounterContended is returned when every compare-and-set attempt lost.
var errCounterContended = errors.New("lockout counter: compare-and-set kept losing to concurrent writers")

// windowCounter is a failure counter kept in a shared cache backend. Each key
// holds "count:windowStartUnixNano" and expires when its window does, so a
// window opens at the first failure and closes a fixed time later, exactly as
// the in-process counters behave. Increments use compare-and-set when the
// backend offers it, which is what makes two replicas failing at the same
// instant both count.
type windowCounter struct {
	backend core.CacheBackend
}

type windowState struct {
	count int64
	start int64 // unix nanos of the first failure in the window
}

func (s windowState) encode() []byte {
	return []byte(strconv.FormatInt(s.count, 10) + ":" + strconv.FormatInt(s.start, 10))
}

func decodeWindowState(b []byte) (windowState, bool) {
	i := bytes.IndexByte(b, ':')
	if i <= 0 {
		return windowState{}, false
	}
	count, err1 := strconv.ParseInt(string(b[:i]), 10, 64)
	start, err2 := strconv.ParseInt(string(b[i+1:]), 10, 64)
	if err1 != nil || err2 != nil || count < 0 {
		return windowState{}, false
	}
	return windowState{count: count, start: start}, true
}

// unreachable reports whether a failed Get means the backend could not be
// reached rather than the key being absent. core.CacheBackend.Get returns one
// opaque error for both, so the backend is asked directly: one that answers a
// ping is up, and the Get that failed against it was a miss.
func (c windowCounter) unreachable(ctx context.Context) bool {
	if p, ok := c.backend.(interface{ Ping(context.Context) error }); ok {
		return p.Ping(ctx) != nil
	}
	return c.backend.Set(ctx, sharedLockoutKeyspace+"health", []byte("1"), 30*time.Second) != nil
}

// read returns the raw stored bytes and the decoded state. present is false
// for a missing key and for a value that does not decode. Raw is still
// returned in the second case so a compare-and-set can replace it.
func (c windowCounter) read(ctx context.Context, key string) (raw []byte, st windowState, present bool, err error) {
	raw, err = c.backend.Get(ctx, key)
	if err != nil {
		if c.unreachable(ctx) {
			return nil, windowState{}, false, fmt.Errorf("read lockout counter: %w", err)
		}
		return nil, windowState{}, false, nil
	}
	st, present = decodeWindowState(raw)
	return raw, st, present, nil
}

// current returns the failure count of the window still open at now, or zero.
func (c windowCounter) current(ctx context.Context, key string, window time.Duration, now time.Time) (windowState, error) {
	_, st, present, err := c.read(ctx, key)
	if err != nil || !present {
		return windowState{}, err
	}
	if now.UnixNano()-st.start > int64(window) {
		return windowState{}, nil
	}
	return st, nil
}

// add records one failure and returns the new state.
func (c windowCounter) add(ctx context.Context, key string, window time.Duration) (windowState, error) {
	cas, hasCAS := c.backend.(auth.CASBackend)
	for attempt := 0; attempt < maxCounterCASAttempts; attempt++ {
		raw, st, present, err := c.read(ctx, key)
		if err != nil {
			return windowState{}, err
		}
		now := time.Now().UnixNano()
		if !present || now-st.start > int64(window) {
			st = windowState{start: now}
		}
		st.count++
		// The key must die with its window, not a full window after the
		// latest failure, or a steady trickle of attempts would keep the
		// window open forever.
		ttl := time.Duration(int64(window) - (now - st.start))
		if ttl <= 0 {
			ttl = window
		}
		next := st.encode()
		if !hasCAS {
			if err := c.backend.Set(ctx, key, next, ttl); err != nil {
				return windowState{}, fmt.Errorf("write lockout counter: %w", err)
			}
			return st, nil
		}
		err = cas.CompareAndSet(ctx, key, raw, next, ttl)
		if err == nil {
			return st, nil
		}
		// A lost race is the common failure and is retried. Backends report
		// it with their own sentinel, so anything that is not a reachability
		// failure is treated as one.
		if !errors.Is(err, auth.ErrCASFailed) && c.unreachable(ctx) {
			return windowState{}, fmt.Errorf("write lockout counter: %w", err)
		}
	}
	return windowState{}, errCounterContended
}

// SharedLockout is the password-login failure tracker backed by a shared
// cache backend. Every replica pointed at the same backend reads and writes
// the same counter, so the attempt limit is one limit for the deployment and
// a restart does not lift a lockout.
type SharedLockout struct {
	counter windowCounter
	window  time.Duration
}

// NewSharedLockout returns a lockout checker that keeps its counters in
// backend. A zero window falls back to DefaultLockoutWindow.
func NewSharedLockout(backend core.CacheBackend, window time.Duration) *SharedLockout {
	if window <= 0 {
		window = DefaultLockoutWindow
	}
	return &SharedLockout{counter: windowCounter{backend: backend}, window: window}
}

func (l *SharedLockout) key(userID uuid.UUID) string {
	return sharedLockoutKeyspace + "login:" + userID.String()
}

// CountRecentFailures returns the failures in the user's open window, or zero
// when the window started before since. A backend that cannot be reached is
// an error, which the login handlers treat as a refusal.
func (l *SharedLockout) CountRecentFailures(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	st, err := l.counter.current(ctx, l.key(userID), l.window, time.Now())
	if err != nil {
		return 0, err
	}
	if st.count == 0 || time.Unix(0, st.start).Before(since) {
		return 0, nil
	}
	return int(st.count), nil
}

// RecordFailedAttempt adds one failure to the user's window.
func (l *SharedLockout) RecordFailedAttempt(ctx context.Context, userID uuid.UUID) error {
	_, err := l.counter.add(ctx, l.key(userID), l.window)
	return err
}

// MFALockoutChecker enforces the MFA verification limits: a per-user failure
// cap, a per-challenge attempt cap, and single use of a challenge token.
// AuthHandler holds the in-process implementation by default and the shared
// one when the runtime has a distributed backend.
type MFALockoutChecker interface {
	CheckMFALockout(ctx context.Context, userID uuid.UUID) (bool, error)
	RecordMFAFailure(ctx context.Context, userID uuid.UUID) error
	CheckChallengeLimit(ctx context.Context, challengeHash string) (bool, error)
	RecordChallengeAttempt(ctx context.Context, challengeHash string) error
	// IsJTIUsed reports whether a challenge token id was already consumed.
	// When the state cannot be read it returns true with the error, so a
	// caller that only looks at the bool still refuses the token.
	IsJTIUsed(ctx context.Context, jti string) (bool, error)
	MarkJTIUsed(ctx context.Context, jti string) error
}

// processMFALockout adapts InMemoryMFALockout to MFALockoutChecker.
type processMFALockout struct {
	mem *InMemoryMFALockout
}

func (p processMFALockout) CheckMFALockout(_ context.Context, userID uuid.UUID) (bool, error) {
	return p.mem.CheckMFALockout(userID)
}

func (p processMFALockout) RecordMFAFailure(_ context.Context, userID uuid.UUID) error {
	p.mem.RecordMFAFailure(userID)
	return nil
}

func (p processMFALockout) CheckChallengeLimit(_ context.Context, challengeHash string) (bool, error) {
	return p.mem.CheckChallengeLimit(challengeHash)
}

func (p processMFALockout) RecordChallengeAttempt(_ context.Context, challengeHash string) error {
	p.mem.RecordChallengeAttempt(challengeHash)
	return nil
}

func (p processMFALockout) IsJTIUsed(_ context.Context, jti string) (bool, error) {
	return p.mem.IsJTIUsed(jti), nil
}

func (p processMFALockout) MarkJTIUsed(_ context.Context, jti string) error {
	p.mem.MarkJTIUsed(jti)
	return nil
}

// SharedMFALockout is the MFA limiter backed by a shared cache backend, with
// the same limits as InMemoryMFALockout. The used-token set lives there too,
// so a challenge consumed on one replica is refused on every other.
type SharedMFALockout struct {
	counter              windowCounter
	maxAttempts          int
	window               time.Duration
	maxChallengeAttempts int
}

// NewSharedMFALockout returns an MFA limiter that keeps its state in backend.
// Zero limits fall back to the package defaults.
func NewSharedMFALockout(backend core.CacheBackend, maxAttempts int, window time.Duration) *SharedMFALockout {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMFAVerifyMaxAttempts
	}
	if window <= 0 {
		window = DefaultMFALockoutWindow
	}
	return &SharedMFALockout{
		counter:              windowCounter{backend: backend},
		maxAttempts:          maxAttempts,
		window:               window,
		maxChallengeAttempts: DefaultMFAChallengeMaxAttempts,
	}
}

func (l *SharedMFALockout) userKey(userID uuid.UUID) string {
	return sharedLockoutKeyspace + "mfa:user:" + userID.String()
}

func (l *SharedMFALockout) challengeKey(challengeHash string) string {
	return sharedLockoutKeyspace + "mfa:challenge:" + challengeHash
}

func (l *SharedMFALockout) jtiKey(jti string) string {
	return sharedLockoutKeyspace + "mfa:jti:" + jti
}

// CheckMFALockout reports whether the user has reached the failure cap.
func (l *SharedMFALockout) CheckMFALockout(ctx context.Context, userID uuid.UUID) (bool, error) {
	st, err := l.counter.current(ctx, l.userKey(userID), l.window, time.Now())
	if err != nil {
		return false, err
	}
	return st.count >= int64(l.maxAttempts), nil
}

// RecordMFAFailure adds one failure to the user's window.
func (l *SharedMFALockout) RecordMFAFailure(ctx context.Context, userID uuid.UUID) error {
	_, err := l.counter.add(ctx, l.userKey(userID), l.window)
	return err
}

// CheckChallengeLimit reports whether the challenge has used its attempts.
func (l *SharedMFALockout) CheckChallengeLimit(ctx context.Context, challengeHash string) (bool, error) {
	st, err := l.counter.current(ctx, l.challengeKey(challengeHash), challengeTokenTTL, time.Now())
	if err != nil {
		return false, err
	}
	return st.count >= int64(l.maxChallengeAttempts), nil
}

// RecordChallengeAttempt spends one of the challenge's attempts.
func (l *SharedMFALockout) RecordChallengeAttempt(ctx context.Context, challengeHash string) error {
	_, err := l.counter.add(ctx, l.challengeKey(challengeHash), challengeTokenTTL)
	return err
}

// IsJTIUsed reports whether the challenge token id was consumed. An
// unreadable state answers true: an outage must not reopen a used token.
func (l *SharedMFALockout) IsJTIUsed(ctx context.Context, jti string) (bool, error) {
	_, err := l.counter.backend.Get(ctx, l.jtiKey(jti))
	if err == nil {
		return true, nil
	}
	if l.counter.unreachable(ctx) {
		return true, fmt.Errorf("read used challenge: %w", err)
	}
	return false, nil
}

// MarkJTIUsed records the challenge token id as consumed for the lifetime of
// a challenge token, after which the token has expired on its own.
func (l *SharedMFALockout) MarkJTIUsed(ctx context.Context, jti string) error {
	if err := l.counter.backend.Set(ctx, l.jtiKey(jti), []byte("1"), challengeTokenTTL); err != nil {
		return fmt.Errorf("mark used challenge: %w", err)
	}
	return nil
}

var (
	_ LockoutChecker    = (*SharedLockout)(nil)
	_ MFALockoutChecker = (*SharedMFALockout)(nil)
	_ MFALockoutChecker = processMFALockout{}
)
