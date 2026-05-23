package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

const lockoutTestSecret = "test-secret-at-least-32-bytes-long!!"

// replicaPair builds two auth handlers standing in for two replicas. With a
// backend they share it. Without one each keeps its own in-process lockouts.
func replicaPair(users userStore, mfa security.MFAStore, backend core.CacheBackend) (*AuthHandler, *AuthHandler) {
	h1 := NewAuthHandler(users, mfa, nil, lockoutTestSecret, 3600, false)
	h2 := NewAuthHandler(users, mfa, nil, lockoutTestSecret, 3600, false)
	h1.WithSharedLockoutBackend(backend)
	h2.WithSharedLockoutBackend(backend)
	return h1, h2
}

func oneUser(t *testing.T) (*fakeUserStore, uuid.UUID) {
	t.Helper()
	uid := uuid.New()
	hash, err := auth.HashPassword("bcrypt", "correct-horse-battery-staple")
	require.NoError(t, err)
	return &fakeUserStore{byEmail: map[string]*domain.User{
		"user@test.com": {ID: uid, Email: "user@test.com", PasswordHash: hash},
	}}, uid
}

func failLogin(t *testing.T, h *AuthHandler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
		strings.NewReader(makeLoginBody("user@test.com", "wrong-password")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)
	require.Equal(t, http.StatusUnauthorized, rr.Code)
}

// splitFailuresLocks spends the attempt limit across two replicas, three
// wrong passwords on one and the rest on the other, and reports whether the
// second replica then counts the user as locked.
func splitFailuresLocks(t *testing.T, h1, h2 *AuthHandler, uid uuid.UUID) bool {
	t.Helper()
	for i := 0; i < 3; i++ {
		failLogin(t, h1)
	}
	for i := 3; i < h2.MaxFailedAttempts; i++ {
		failLogin(t, h2)
	}
	n, err := h2.lockoutChecker.CountRecentFailures(context.Background(), uid, time.Now().Add(-h2.LockoutWindow))
	require.NoError(t, err)
	return n >= h2.MaxFailedAttempts
}

func TestSharedLockout_TwoReplicasEnforceOneLimit(t *testing.T) {
	t.Parallel()
	users, uid := oneUser(t)
	h1, h2 := replicaPair(users, nil, auth.NewFlushProtectedBackend(auth.NewMemoryBackend()))

	assert.True(t, splitFailuresLocks(t, h1, h2, uid),
		"failures on two replicas sharing a backend must add up to one limit")
}

// The negative control: the same sequence against the in-process lockouts
// leaves the account open, which is the defect the shared backend removes.
func TestInMemoryLockout_TwoReplicasCountSeparately(t *testing.T) {
	t.Parallel()
	users, uid := oneUser(t)
	h1, h2 := replicaPair(users, nil, nil)

	assert.False(t, splitFailuresLocks(t, h1, h2, uid),
		"in-process lockouts count per replica, so the split sequence must not lock")
}

func TestSharedLockout_RestartKeepsLockout(t *testing.T) {
	t.Parallel()
	users, uid := oneUser(t)
	backend := auth.NewFlushProtectedBackend(auth.NewMemoryBackend())
	before, _ := replicaPair(users, nil, backend)
	for i := 0; i < before.MaxFailedAttempts; i++ {
		failLogin(t, before)
	}

	after := NewAuthHandler(users, nil, nil, lockoutTestSecret, 3600, false)
	after.WithSharedLockoutBackend(backend)
	n, err := after.lockoutChecker.CountRecentFailures(context.Background(), uid, time.Now().Add(-after.LockoutWindow))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, after.MaxFailedAttempts, "a new process on the same backend must still see the lockout")
}

func TestSharedLockout_WindowStartedBeforeSinceCountsZero(t *testing.T) {
	t.Parallel()
	l := NewSharedLockout(auth.NewMemoryBackend(), time.Hour)
	uid := uuid.New()
	require.NoError(t, l.RecordFailedAttempt(context.Background(), uid))

	n, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestSharedLockout_ConcurrentReplicasCountEveryFailure(t *testing.T) {
	t.Parallel()
	backend := auth.NewFlushProtectedBackend(auth.NewMemoryBackend())
	a := NewSharedLockout(backend, time.Hour)
	b := NewSharedLockout(backend, time.Hour)
	uid := uuid.New()

	// Each lost compare-and-set means another writer won, so with fewer
	// writers than the retry bound every one of them lands.
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		l := a
		if i%2 == 1 {
			l = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.RecordFailedAttempt(context.Background(), uid)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	n, err := a.CountRecentFailures(context.Background(), uid, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, writers, n)
}

// racingBackend lets another replica write the counter between this
// replica's read and its compare-and-set, once.
type racingBackend struct {
	*auth.MemoryBackend
	once sync.Once
	race func()
	casN atomic.Int32
}

func (r *racingBackend) CompareAndSet(ctx context.Context, key string, oldVal, newVal []byte, ttl time.Duration) error {
	r.casN.Add(1)
	r.once.Do(r.race)
	return r.MemoryBackend.CompareAndSet(ctx, key, oldVal, newVal, ttl)
}

func TestSharedLockout_CASContentionCountsBothFailures(t *testing.T) {
	t.Parallel()
	mem := auth.NewMemoryBackend()
	other := NewSharedLockout(mem, time.Hour)
	uid := uuid.New()
	rb := &racingBackend{MemoryBackend: mem}
	rb.race = func() { require.NoError(t, other.RecordFailedAttempt(context.Background(), uid)) }
	l := NewSharedLockout(rb, time.Hour)

	require.NoError(t, l.RecordFailedAttempt(context.Background(), uid))

	n, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the failure that lost the race must be retried, not dropped")
	assert.Equal(t, int32(2), rb.casN.Load(), "one lost compare-and-set, one retry")
}

// setOnlyBackend has no compare-and-set, so the counter uses Get then Set.
type setOnlyBackend struct{ mem *auth.MemoryBackend }

func (b setOnlyBackend) Get(ctx context.Context, k string) ([]byte, error) { return b.mem.Get(ctx, k) }
func (b setOnlyBackend) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	return b.mem.Set(ctx, k, v, ttl)
}
func (b setOnlyBackend) Delete(ctx context.Context, k string) error { return b.mem.Delete(ctx, k) }
func (b setOnlyBackend) Flush(ctx context.Context) error            { return b.mem.Flush(ctx) }

func TestSharedLockout_BackendWithoutCASStillCounts(t *testing.T) {
	t.Parallel()
	l := NewSharedLockout(setOnlyBackend{mem: auth.NewMemoryBackend()}, time.Hour)
	uid := uuid.New()
	for i := 0; i < 3; i++ {
		require.NoError(t, l.RecordFailedAttempt(context.Background(), uid))
	}
	n, err := l.CountRecentFailures(context.Background(), uid, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

// downBackend is a cache backend whose server cannot be reached.
type downBackend struct{}

var errBackendDown = errors.New("dial tcp: connection refused")

func (downBackend) Get(context.Context, string) ([]byte, error)              { return nil, errBackendDown }
func (downBackend) Set(context.Context, string, []byte, time.Duration) error { return errBackendDown }
func (downBackend) Delete(context.Context, string) error                     { return errBackendDown }
func (downBackend) Flush(context.Context) error                              { return errBackendDown }
func (downBackend) Ping(context.Context) error                               { return errBackendDown }
func (downBackend) CompareAndSet(context.Context, string, []byte, []byte, time.Duration) error {
	return errBackendDown
}

func TestSharedLockout_UnreachableBackendIsAnError(t *testing.T) {
	t.Parallel()
	l := NewSharedLockout(downBackend{}, time.Hour)
	_, err := l.CountRecentFailures(context.Background(), uuid.New(), time.Now().Add(-time.Hour))
	require.Error(t, err, "an outage must not read as zero failures")
	require.Error(t, l.RecordFailedAttempt(context.Background(), uuid.New()))
}

func mfaVerify(h *AuthHandler, challengeToken, code string) int {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify",
		strings.NewReader(makeMFAVerifyBody(challengeToken, code)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, req)
	return rr.Code
}

func mfaFixture(t *testing.T) (security.MFAStore, string) {
	t.Helper()
	totpSecret := "JBSWY3DPEHPK3PXP"
	enc, err := security.EncryptSecret(totpSecret, lockoutTestSecret)
	require.NoError(t, err)
	return &fakeMFAStoreWithEncryptedSecret{encSecret: enc}, totpSecret
}

func TestSharedMFALockout_ChallengeUsedOnOneReplicaRefusedOnAnother(t *testing.T) {
	t.Parallel()
	mfa, totpSecret := mfaFixture(t)
	h1, h2 := replicaPair(nil, mfa, auth.NewFlushProtectedBackend(auth.NewMemoryBackend()))

	code, err := totp.GenerateCode(totpSecret, time.Now())
	require.NoError(t, err)
	challenge, err := auth.SignChallenge(lockoutTestSecret, uuid.New(), "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, mfaVerify(h1, challenge, code))
	assert.Equal(t, http.StatusUnauthorized, mfaVerify(h2, challenge, code),
		"a challenge consumed on one replica must be refused on the other")
}

func TestSharedMFALockout_JTIMarkedOnOneSeenOnAnother(t *testing.T) {
	t.Parallel()
	backend := auth.NewFlushProtectedBackend(auth.NewMemoryBackend())
	a := NewSharedMFALockout(backend, 0, 0)
	b := NewSharedMFALockout(backend, 0, 0)
	ctx := context.Background()

	used, err := b.IsJTIUsed(ctx, "jti-1")
	require.NoError(t, err)
	require.False(t, used)

	require.NoError(t, a.MarkJTIUsed(ctx, "jti-1"))
	used, err = b.IsJTIUsed(ctx, "jti-1")
	require.NoError(t, err)
	assert.True(t, used)
}

func TestSharedMFALockout_FailuresOnTwoReplicasLockTheUser(t *testing.T) {
	t.Parallel()
	mfa, _ := mfaFixture(t)
	h1, h2 := replicaPair(nil, mfa, auth.NewFlushProtectedBackend(auth.NewMemoryBackend()))
	uid := uuid.New()

	// A fresh challenge per attempt, so only the per-user cap can trip.
	for i := 0; i < DefaultMFAVerifyMaxAttempts; i++ {
		h := h1
		if i >= 3 {
			h = h2
		}
		challenge, err := auth.SignChallenge(lockoutTestSecret, uid, "test@example.com", []string{"admin"}, "")
		require.NoError(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, mfaVerify(h, challenge, "000000"), "attempt %d", i+1)
	}

	challenge, err := auth.SignChallenge(lockoutTestSecret, uid, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)
	assert.Equal(t, http.StatusTooManyRequests, mfaVerify(h1, challenge, "000000"))
}

func TestSharedMFALockout_ChallengeAttemptsShared(t *testing.T) {
	t.Parallel()
	backend := auth.NewMemoryBackend()
	a := NewSharedMFALockout(backend, 0, 0)
	b := NewSharedMFALockout(backend, 0, 0)
	ctx := context.Background()
	for i := 0; i < DefaultMFAChallengeMaxAttempts; i++ {
		l := a
		if i%2 == 1 {
			l = b
		}
		require.NoError(t, l.RecordChallengeAttempt(ctx, "hash"))
	}
	exhausted, err := a.CheckChallengeLimit(ctx, "hash")
	require.NoError(t, err)
	assert.True(t, exhausted)
}

func TestSharedMFALockout_UnreachableBackendRefusesChallenge(t *testing.T) {
	t.Parallel()
	mfa, totpSecret := mfaFixture(t)
	h := NewAuthHandler(nil, mfa, nil, lockoutTestSecret, 3600, false)
	h.WithSharedLockoutBackend(downBackend{})

	used, err := h.mfaLockoutChecker.IsJTIUsed(context.Background(), "any")
	require.Error(t, err)
	assert.True(t, used, "an unreadable replay state must read as used")

	code, err := totp.GenerateCode(totpSecret, time.Now())
	require.NoError(t, err)
	challenge, err := auth.SignChallenge(lockoutTestSecret, uuid.New(), "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, mfaVerify(h, challenge, code),
		"a valid code must not get through while the replay guard is down")
}

func TestAuthHandler_WithSharedLockoutBackendNilKeepsInProcess(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, lockoutTestSecret, 3600, false)
	h.WithSharedLockoutBackend(nil)

	_, ok := h.lockoutChecker.(*InMemoryLockout)
	assert.True(t, ok)
	_, ok = h.mfaLockoutChecker.(processMFALockout)
	assert.True(t, ok)
}
