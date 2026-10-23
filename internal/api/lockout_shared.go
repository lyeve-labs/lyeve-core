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

// windowCounter is an attempt counter kept in a shared cache backend. Each key
// holds "count:windowStartUnixNano" and expires when its window does, so a
// window opens at the first attempt and closes a fixed time later, exactly as
// the in-process counters behave. Increments use compare-and-set when the
// backend offers it, which is what makes two replicas counting at the same
// instant both count, and what lets the limit be checked in the same write.
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

// reserve counts one attempt unless the open window already holds limit, and
// returns the window's state and whether the attempt was counted. The limit is
// read from the value the compare-and-set replaces, so of many concurrent
// callers on any replica only limit are counted. A backend without
// compare-and-set is read and then written, which concurrent callers can both
// pass.
func (c windowCounter) reserve(ctx context.Context, key string, window time.Duration, limit int) (windowState, bool, error) {
	cas, hasCAS := c.backend.(auth.CASBackend)
	for attempt := 0; attempt < maxCounterCASAttempts; attempt++ {
		raw, st, present, err := c.read(ctx, key)
		if err != nil {
			return windowState{}, false, err
		}
		now := time.Now().UnixNano()
		if !present || now-st.start > int64(window) {
			st = windowState{start: now}
		}
		if st.count >= int64(limit) {
			return st, false, nil
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
				return windowState{}, false, fmt.Errorf("write lockout counter: %w", err)
			}
			return st, true, nil
		}
		err = cas.CompareAndSet(ctx, key, raw, next, ttl)
		if err == nil {
			return st, true, nil
		}
		// A lost race is the common failure and is retried. Backends report
		// it with their own sentinel, so anything that is not a reachability
		// failure is treated as one.
		if !errors.Is(err, auth.ErrCASFailed) && c.unreachable(ctx) {
			return windowState{}, false, fmt.Errorf("write lockout counter: %w", err)
		}
	}
	return windowState{}, false, errCounterContended
}

// release gives back one attempt counted in the window that opened at start.
// A window that has closed or been replaced since is left as it is, and the
// count never goes below zero. The decrement is a compare-and-set from the
// stored value, so it takes back exactly one attempt however many replicas
// write at once.
func (c windowCounter) release(ctx context.Context, key string, window time.Duration, start int64) error {
	cas, hasCAS := c.backend.(auth.CASBackend)
	for attempt := 0; attempt < maxCounterCASAttempts; attempt++ {
		raw, st, present, err := c.read(ctx, key)
		if err != nil {
			return err
		}
		ttl := time.Duration(int64(window) - (time.Now().UnixNano() - st.start))
		if !present || st.start != start || st.count == 0 || ttl <= 0 {
			return nil
		}
		st.count--
		next := st.encode()
		if !hasCAS {
			if err := c.backend.Set(ctx, key, next, ttl); err != nil {
				return fmt.Errorf("write lockout counter: %w", err)
			}
			return nil
		}
		err = cas.CompareAndSet(ctx, key, raw, next, ttl)
		if err == nil {
			return nil
		}
		if !errors.Is(err, auth.ErrCASFailed) && c.unreachable(ctx) {
			return fmt.Errorf("write lockout counter: %w", err)
		}
	}
	return errCounterContended
}

// clear deletes the counter, which closes its window.
func (c windowCounter) clear(ctx context.Context, key string) error {
	if err := c.backend.Delete(ctx, key); err != nil {
		return fmt.Errorf("clear lockout counter: %w", err)
	}
	return nil
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

// ReserveAttempt counts one password attempt against the user before the
// password is checked, unless the window already holds limit attempts, and
// returns the window's count and whether the attempt was counted. Losing every
// compare-and-set is an error, which the handlers treat as a refusal.
func (l *SharedLockout) ReserveAttempt(ctx context.Context, userID uuid.UUID, limit int) (int, bool, error) {
	st, ok, err := l.counter.reserve(ctx, l.key(userID), l.window, limit)
	return int(st.count), ok, err
}

// ResetFailedAttempts clears the user's window after a correct password.
func (l *SharedLockout) ResetFailedAttempts(ctx context.Context, userID uuid.UUID) error {
	return l.counter.clear(ctx, l.key(userID))
}

// MFALockoutChecker enforces the MFA verification limits: a per-user failure
// cap, a per-challenge attempt cap, and single use of a challenge token.
// AuthHandler holds the in-process implementation by default and the shared
// one when the runtime has a distributed backend.
type MFALockoutChecker interface {
	CheckMFALockout(ctx context.Context, userID uuid.UUID) (bool, error)
	// ReserveMFAAttempt counts one code attempt against the user before the
	// code is checked, and reports false without counting once the cap is
	// reached. ResetMFAAttempts clears the count after a code that verifies.
	ReserveMFAAttempt(ctx context.Context, userID uuid.UUID) (Reservation, bool, error)
	// ReleaseMFAAttempt gives back one reserved attempt whose code was never
	// judged, because the store failed while spending it.
	ReleaseMFAAttempt(ctx context.Context, userID uuid.UUID, r Reservation) error
	ResetMFAAttempts(ctx context.Context, userID uuid.UUID) error
	// ReserveChallengeAttempt spends one of the challenge's attempts, and
	// reports false without spending once they are used up.
	ReserveChallengeAttempt(ctx context.Context, challengeHash string) (bool, error)
	// IsJTIUsed reports whether a challenge token id was already consumed.
	// When the state cannot be read it returns true with the error, so a
	// caller that only looks at the bool still refuses the token.
	IsJTIUsed(ctx context.Context, jti string) (bool, error)
	// MarkJTIUsed records a challenge token id as consumed and reports
	// whether this call recorded it. Of two calls for one id only one gets
	// true, so a caller that spends the challenge on true completes it once.
	MarkJTIUsed(ctx context.Context, jti string) (bool, error)
}

// processMFALockout adapts InMemoryMFALockout to MFALockoutChecker.
type processMFALockout struct {
	mem *InMemoryMFALockout
}

func (p processMFALockout) CheckMFALockout(_ context.Context, userID uuid.UUID) (bool, error) {
	return p.mem.CheckMFALockout(userID)
}

func (p processMFALockout) ReserveMFAAttempt(_ context.Context, userID uuid.UUID) (Reservation, bool, error) {
	r, ok := p.mem.ReserveMFAAttempt(userID)
	return r, ok, nil
}

func (p processMFALockout) ReleaseMFAAttempt(_ context.Context, userID uuid.UUID, r Reservation) error {
	p.mem.ReleaseMFAAttempt(userID, r)
	return nil
}

func (p processMFALockout) ResetMFAAttempts(_ context.Context, userID uuid.UUID) error {
	p.mem.ResetMFAAttempts(userID)
	return nil
}

func (p processMFALockout) ReserveChallengeAttempt(_ context.Context, challengeHash string) (bool, error) {
	return p.mem.ReserveChallengeAttempt(challengeHash), nil
}

func (p processMFALockout) IsJTIUsed(_ context.Context, jti string) (bool, error) {
	return p.mem.IsJTIUsed(jti), nil
}

func (p processMFALockout) MarkJTIUsed(_ context.Context, jti string) (bool, error) {
	return p.mem.MarkJTIUsed(jti), nil
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

// ReserveMFAAttempt counts one code attempt against the user unless the
// failure cap is reached.
func (l *SharedMFALockout) ReserveMFAAttempt(ctx context.Context, userID uuid.UUID) (Reservation, bool, error) {
	st, ok, err := l.counter.reserve(ctx, l.userKey(userID), l.window, l.maxAttempts)
	return Reservation{Count: int(st.count), window: st.start}, ok, err
}

// ReleaseMFAAttempt gives back one attempt reserved in the user's window.
func (l *SharedMFALockout) ReleaseMFAAttempt(ctx context.Context, userID uuid.UUID, r Reservation) error {
	return l.counter.release(ctx, l.userKey(userID), l.window, r.window)
}

// ResetMFAAttempts clears the user's failures after a code that verifies.
func (l *SharedMFALockout) ResetMFAAttempts(ctx context.Context, userID uuid.UUID) error {
	return l.counter.clear(ctx, l.userKey(userID))
}

// ReserveChallengeAttempt spends one of the challenge's attempts unless they
// are used up.
func (l *SharedMFALockout) ReserveChallengeAttempt(ctx context.Context, challengeHash string) (bool, error) {
	_, ok, err := l.counter.reserve(ctx, l.challengeKey(challengeHash), challengeTokenTTL, l.maxChallengeAttempts)
	return ok, err
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
// a challenge token, after which the token has expired on its own. A backend
// with compare-and-set creates the key only when it is absent, so one replica
// wins. Any refusal from a reachable backend is read as a lost race, since
// backends report a lost compare-and-set with their own sentinels. A backend
// without compare-and-set is read and then written, which two replicas can
// both pass at the same instant.
func (l *SharedMFALockout) MarkJTIUsed(ctx context.Context, jti string) (bool, error) {
	key := l.jtiKey(jti)
	if cas, ok := l.counter.backend.(auth.CASBackend); ok {
		err := cas.CompareAndSet(ctx, key, nil, []byte("1"), challengeTokenTTL)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, auth.ErrCASFailed) && l.counter.unreachable(ctx) {
			return false, fmt.Errorf("mark used challenge: %w", err)
		}
		return false, nil
	}
	used, err := l.IsJTIUsed(ctx, jti)
	if err != nil || used {
		return false, err
	}
	if err := l.counter.backend.Set(ctx, key, []byte("1"), challengeTokenTTL); err != nil {
		return false, fmt.Errorf("mark used challenge: %w", err)
	}
	return true, nil
}

var (
	_ LockoutChecker    = (*SharedLockout)(nil)
	_ MFALockoutChecker = (*SharedMFALockout)(nil)
	_ MFALockoutChecker = processMFALockout{}
)
