package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// inProcessMFA returns the in-process MFA lockout a default AuthHandler holds.
func inProcessMFA(t *testing.T, h *AuthHandler) *InMemoryMFALockout {
	t.Helper()
	p, ok := h.mfaLockoutChecker.(processMFALockout)
	require.True(t, ok, "default MFA lockout should be the in-process one")
	return p.mem
}

// InMemoryMFALockout: unit tests

func TestInMemoryMFALockout_CheckMFALockout_NoFailures(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	uid := uuid.New()

	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.False(t, locked, "user with no failures should not be locked out")
}

func TestInMemoryMFALockout_CheckMFALockout_UnderThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 4; i++ {
		l.RecordMFAFailure(uid)
	}

	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.False(t, locked, "4 failures with limit=5 should NOT lock out")
}

func TestInMemoryMFALockout_CheckMFALockout_AtThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		l.RecordMFAFailure(uid)
	}

	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.True(t, locked, "5 failures with limit=5 should lock out")
}

func TestInMemoryMFALockout_CheckMFALockout_AboveThreshold(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 10; i++ {
		l.RecordMFAFailure(uid)
	}

	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.True(t, locked, "10 failures with limit=5 should remain locked out")
}

func TestInMemoryMFALockout_CheckMFALockout_WindowExpiry(t *testing.T) {
	t.Parallel()

	// Use a very short window so we can test expiry.
	l := NewInMemoryMFALockout(5, 10*time.Millisecond)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		l.RecordMFAFailure(uid)
	}

	// Should be locked immediately.
	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.True(t, locked, "should be locked right after 5 failures")

	// Wait for window to expire.
	time.Sleep(20 * time.Millisecond)

	locked, err = l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.False(t, locked, "should unlock after window expires")
}

func TestInMemoryMFALockout_ResetMFAAttempts(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	uid := uuid.New()

	for i := 0; i < 5; i++ {
		l.RecordMFAFailure(uid)
	}

	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.True(t, locked, "should be locked after 5 failures")

	l.ResetMFAAttempts(uid)

	locked, err = l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.False(t, locked, "should unlock after reset")
}

func TestInMemoryMFALockout_PerUserIsolation(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	uid1 := uuid.New()
	uid2 := uuid.New()

	// Lock out uid1.
	for i := 0; i < 5; i++ {
		l.RecordMFAFailure(uid1)
	}

	locked1, err := l.CheckMFALockout(uid1)
	require.NoError(t, err)
	assert.True(t, locked1, "uid1 should be locked")

	locked2, err := l.CheckMFALockout(uid2)
	require.NoError(t, err)
	assert.False(t, locked2, "uid2 should NOT be locked - different user")
}

func TestInMemoryMFALockout_WindowResetOnExpiry(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 10*time.Millisecond)
	uid := uuid.New()

	// 3 failures in first window.
	for i := 0; i < 3; i++ {
		l.RecordMFAFailure(uid)
	}

	time.Sleep(20 * time.Millisecond) // window expires

	// 3 more failures in new window: should NOT lock (3 < 5).
	for i := 0; i < 3; i++ {
		l.RecordMFAFailure(uid)
	}

	locked, err := l.CheckMFALockout(uid)
	require.NoError(t, err)
	assert.False(t, locked, "new window should reset counter - 3 < 5")
}

// Challenge-level attempt cap

func TestInMemoryMFALockout_CheckChallengeLimit_NoAttempts(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)

	exhausted, err := l.CheckChallengeLimit("unknown-challenge-hash")
	require.NoError(t, err)
	assert.False(t, exhausted, "unused challenge should not be exhausted")
}

func TestInMemoryMFALockout_CheckChallengeLimit_UnderCap(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	ch := "test-challenge-hash"

	for i := 0; i < 4; i++ {
		l.RecordChallengeAttempt(ch)
	}

	exhausted, err := l.CheckChallengeLimit(ch)
	require.NoError(t, err)
	assert.False(t, exhausted, "4 attempts with cap=5 should not be exhausted")
}

func TestInMemoryMFALockout_CheckChallengeLimit_AtCap(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	ch := "test-challenge-hash"

	for i := 0; i < 5; i++ {
		l.RecordChallengeAttempt(ch)
	}

	exhausted, err := l.CheckChallengeLimit(ch)
	require.NoError(t, err)
	assert.True(t, exhausted, "5 attempts with cap=5 should be exhausted")
}

func TestInMemoryMFALockout_CheckChallengeLimit_AboveCap(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	ch := "test-challenge-hash"

	for i := 0; i < 10; i++ {
		l.RecordChallengeAttempt(ch)
	}

	exhausted, err := l.CheckChallengeLimit(ch)
	require.NoError(t, err)
	assert.True(t, exhausted, "10 attempts with cap=5 should remain exhausted")
}

func TestInMemoryMFALockout_CheckChallengeLimit_PerChallengeIsolation(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	ch1 := "challenge-hash-1"
	ch2 := "challenge-hash-2"

	// Exhaust ch1.
	for i := 0; i < 5; i++ {
		l.RecordChallengeAttempt(ch1)
	}

	exhausted1, err := l.CheckChallengeLimit(ch1)
	require.NoError(t, err)
	assert.True(t, exhausted1, "ch1 should be exhausted")

	exhausted2, err := l.CheckChallengeLimit(ch2)
	require.NoError(t, err)
	assert.False(t, exhausted2, "ch2 should NOT be exhausted - different challenge")
}

// Default config

func TestNewInMemoryMFALockout_Defaults(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(0, 0)

	assert.Equal(t, DefaultMFAVerifyMaxAttempts, l.maxAttempts, "should use default max attempts")
	assert.Equal(t, DefaultMFALockoutWindow, l.window, "should use default window")
	assert.Equal(t, DefaultMFAChallengeMaxAttempts, l.maxChallengeAttempts, "should use default challenge max")
}

// AuthHandler wiring: MFAVerify returns 429 when challenge exhausted

func TestMFAVerify_ChallengeExhausted_429(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()
	mfaStore := &fakeMFAStoreWithEncryptedSecret{}

	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	// Create a valid challenge token.
	challengeToken, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err, "SignChallenge")

	// Exhaust the challenge by recording DefaultMFAChallengeMaxAttempts attempts.
	challengeHash := sha256Hex(challengeToken)
	for i := 0; i < DefaultMFAChallengeMaxAttempts; i++ {
		inProcessMFA(t, h).RecordChallengeAttempt(challengeHash)
	}

	// Now try to verify: should get 429.
	body := makeMFAVerifyBody(challengeToken, "000000")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.MFAVerify(rr, req)

	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "exhausted challenge should return 429")

	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	assert.Equal(t, "AUTH_MFA_LOCKED_OUT", resp["code"], "should return MFA locked out error code")
}

func TestMFAVerify_PerUserLockout_429(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()
	mfaStore := &fakeMFAStoreWithEncryptedSecret{}

	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	// Lock out the user by recording MaxAttempts failures.
	for i := 0; i < DefaultMFAVerifyMaxAttempts; i++ {
		inProcessMFA(t, h).RecordMFAFailure(userID)
	}

	// Create a valid challenge token (different from the ones used to lock out).
	challengeToken, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)

	body := makeMFAVerifyBody(challengeToken, "000000")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.MFAVerify(rr, req)

	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "locked-out user should get 429")
}

func TestMFAVerify_RecordsChallengeAttempt(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()
	mfaStore := &fakeMFAStoreWithEncryptedSecret{}

	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	challengeToken, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)

	challengeHash := sha256Hex(challengeToken)

	// Verify no attempts before the call.
	exhausted, err := inProcessMFA(t, h).CheckChallengeLimit(challengeHash)
	require.NoError(t, err)
	assert.False(t, exhausted, "should start with 0 attempts")

	// Make a single MFAVerify call: even though it will fail at decrypt,
	// the challenge attempt should be recorded BEFORE the decrypt step.
	body := makeMFAVerifyBody(challengeToken, "000000")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, req)

	// The attempt was recorded. Verify by checking the counter directly
	// on the InMemoryMFALockout.
	iml := inProcessMFA(t, h)
	iml.mu.Lock()
	counter, exists := iml.challengeEntries[challengeHash]
	iml.mu.Unlock()
	require.True(t, exists, "challenge entry should exist after MFAVerify call")
	assert.Equal(t, int64(1), counter.counter.Load(), "should have recorded exactly 1 attempt")
}

func TestMFAVerify_FailedCode_RecordsUserFailure(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()
	totpSecret := "JBSWY3DPEHPK3PXP"

	// Encrypt the TOTP secret using the legacy security.EncryptSecret so
	// the MFAVerify handler can decrypt it via the legacy fallback path.
	encSecret, err := security.EncryptSecret(totpSecret, secret)
	require.NoError(t, err, "EncryptSecret")

	mfaStore := &fakeMFAStoreWithEncryptedSecret{encSecret: encSecret}

	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	challengeToken, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)

	// Send a bad TOTP code: should return 422 and record a user failure.
	body := makeMFAVerifyBody(challengeToken, "000000")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, req)

	// Should get 422 (invalid code), not 500 or 429.
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code, "bad TOTP code should return 422")

	// Verify the failure was recorded.
	iml := inProcessMFA(t, h)
	iml.mu.Lock()
	state, exists := iml.entries[userID]
	iml.mu.Unlock()
	require.True(t, exists, "user entry should exist after failed MFA attempt")
	assert.Equal(t, int64(1), state.failures.Load(), "should have recorded 1 MFA failure")
}

func TestMFAVerify_MultipleFailures_ThenLockout(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()
	totpSecret := "JBSWY3DPEHPK3PXP"

	encSecret, err := security.EncryptSecret(totpSecret, secret)
	require.NoError(t, err, "EncryptSecret")

	mfaStore := &fakeMFAStoreWithEncryptedSecret{encSecret: encSecret}
	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	// Send 5 bad codes: each uses a fresh challenge to avoid challenge cap.
	// The lockout check runs BEFORE the failure is recorded, so the 5th
	// bad code still returns 422 (failure recorded after the check).
	for i := 0; i < 5; i++ {
		challengeToken, cErr := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
		require.NoError(t, cErr)

		body := makeMFAVerifyBody(challengeToken, "000000")
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.MFAVerify(rr, req)

		assert.Equal(t, http.StatusUnprocessableEntity, rr.Code,
			"attempt %d: bad code should return 422", i+1)
	}

	// User should have 5 failures, now locked.
	locked, err := inProcessMFA(t, h).CheckMFALockout(userID)
	require.NoError(t, err)
	assert.True(t, locked, "5 failures ≥ 5 limit - locked")

	// 6th attempt should get 429 from per-user lockout (check runs before failure recording).
	challengeToken, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err)
	body := makeMFAVerifyBody(challengeToken, "000000")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, req)

	assert.Equal(t, http.StatusTooManyRequests, rr.Code,
		"6th attempt should trigger 429 (user lockout)")
}

// MFAVerify: challenge token single-use

func TestMFAVerify_ChallengeTokenSingleUse_RejectsReplay(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()
	totpSecret := "JBSWY3DPEHPK3PXP" // well-known Base32 TOTP key

	encSecret, err := security.EncryptSecret(totpSecret, secret)
	require.NoError(t, err, "EncryptSecret")

	mfaStore := &fakeMFAStoreWithEncryptedSecret{encSecret: encSecret}
	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	// Generate a valid TOTP code for the current timestep.
	code, err := totp.GenerateCode(totpSecret, time.Now())
	require.NoError(t, err, "GenerateCode")

	// The challenge token carries a jti from SignChallenge.
	challengeToken, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err, "SignChallenge")

	// Parse the token to extract the jti for assertions.
	claims, err := auth.Parse(secret, challengeToken)
	require.NoError(t, err, "parse challenge token")
	require.NotEmpty(t, claims.ID, "challenge token must have a jti")

	// First verification: should succeed
	body1 := makeMFAVerifyBody(challengeToken, code)
	req1 := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	rr1 := httptest.NewRecorder()
	h.MFAVerify(rr1, req1)

	assert.Equal(t, http.StatusOK, rr1.Code, "first verify should succeed")

	// After success, the jti must be marked as consumed.
	assert.True(t, inProcessMFA(t, h).IsJTIUsed(claims.ID),
		"jti should be marked used after successful verification")

	// Second verification: must reject the replay
	body2 := makeMFAVerifyBody(challengeToken, code)
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	h.MFAVerify(rr2, req2)

	assert.Equal(t, http.StatusUnauthorized, rr2.Code,
		"replay of consumed challenge token must be rejected")

	// A fresh challenge does not make the used code good again: a captured
	// code signs in once, whatever challenge carries it.
	challengeToken2, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err, "second SignChallenge")
	verify := func(token, code string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(makeMFAVerifyBody(token, code)))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.MFAVerify(rr, req)
		return rr.Code
	}
	assert.Equal(t, http.StatusUnprocessableEntity, verify(challengeToken2, code),
		"a code already used to sign in must be refused on a new challenge")

	// The next code signs in on a fresh challenge.
	challengeToken3, err := auth.SignChallenge(secret, userID, "test@example.com", []string{"admin"}, "")
	require.NoError(t, err, "third SignChallenge")
	next, err := totp.GenerateCode(totpSecret, time.Now().Add(30*time.Second))
	require.NoError(t, err, "GenerateCode")
	assert.Equal(t, http.StatusOK, verify(challengeToken3, next),
		"a later code on a fresh challenge must succeed")
}

// InMemoryMFALockout: JTI single-use unit tests

func TestInMemoryMFALockout_MarkJTIUsed(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	jti := "test-jti-1"

	assert.False(t, l.IsJTIUsed(jti), "fresh jti should not be used")
	l.MarkJTIUsed(jti)
	assert.True(t, l.IsJTIUsed(jti), "jti should be used after MarkJTIUsed")
}

func TestInMemoryMFALockout_IsJTIUsed_Isolation(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)

	l.MarkJTIUsed("jti-a")
	assert.True(t, l.IsJTIUsed("jti-a"), "jti-a should be used")
	assert.False(t, l.IsJTIUsed("jti-b"), "jti-b should not be used")
}

// ChallengeEntry pruning: expired entries must be cleaned up

func TestInMemoryMFALockout_ChallengeEntryExpiry_PeekPrunes(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	chHash := "expired-challenge-hash"

	// Insert an entry whose createdAt is beyond challengeTokenTTL.
	l.mu.Lock()
	l.challengeEntries[chHash] = &challengeEntryState{
		counter:   new(atomic.Int64),
		createdAt: time.Now().Add(-challengeTokenTTL - time.Second),
	}
	l.mu.Unlock()

	// Peek: the expired entry must be pruned and report not-exhausted.
	exhausted, err := l.CheckChallengeLimit(chHash)
	require.NoError(t, err)
	assert.False(t, exhausted, "expired challenge should not be exhausted (pruned)")

	// The entry must have been removed from the map.
	l.mu.Lock()
	_, exists := l.challengeEntries[chHash]
	l.mu.Unlock()
	assert.False(t, exists, "expired challenge entry must be deleted from map")
}

func TestInMemoryMFALockout_ChallengeEntryExpiry_RecordCreatesFresh(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	chHash := "expired-challenge-hash"

	// Insert an entry whose createdAt is beyond challengeTokenTTL.
	l.mu.Lock()
	l.challengeEntries[chHash] = &challengeEntryState{
		counter:   new(atomic.Int64),
		createdAt: time.Now().Add(-challengeTokenTTL - time.Second),
	}
	// Preload the counter to 5 (at cap).
	l.challengeEntries[chHash].counter.Store(5)
	l.mu.Unlock()

	// Record a new attempt: the expired entry should be pruned and a fresh
	// one created, so the counter starts at 1, not 6.
	l.RecordChallengeAttempt(chHash)

	l.mu.Lock()
	entry, exists := l.challengeEntries[chHash]
	l.mu.Unlock()
	require.True(t, exists, "fresh entry must exist after recording on expired")
	assert.Equal(t, int64(1), entry.counter.Load(), "fresh entry must have counter=1, not 6")

	// Fresh entry should have been created recently.
	assert.WithinDuration(t, time.Now(), entry.createdAt, time.Second,
		"fresh entry must have recent createdAt")
}

func TestInMemoryMFALockout_ChallengeEntryExpiry_FreshEntryNotPruned(t *testing.T) {
	t.Parallel()

	l := NewInMemoryMFALockout(5, 15*time.Minute)
	chHash := "fresh-challenge-hash"

	// Record an attempt on a fresh hash: entry is auto-created.
	l.RecordChallengeAttempt(chHash)

	// Verify it's present and not pruned by CheckChallengeLimit.
	exhausted, err := l.CheckChallengeLimit(chHash)
	require.NoError(t, err)
	assert.False(t, exhausted, "fresh challenge with 1 attempt should not be exhausted")

	l.mu.Lock()
	_, exists := l.challengeEntries[chHash]
	l.mu.Unlock()
	assert.True(t, exists, "fresh challenge entry must survive peek")
}

// Helpers

// fakeMFAStoreWithEncryptedSecret provides a minimal MFAStore that returns
// either an empty string (for 429 tests that never reach decrypt) or a
// properly encrypted TOTP secret (for tests that exercise the full path).
type fakeMFAStoreWithEncryptedSecret struct {
	encSecret string // pre-encrypted via security.EncryptSecret; empty = return ""
	steps     totpStepLog
}

func (s *fakeMFAStoreWithEncryptedSecret) ConsumeTOTPStep(_ context.Context, userID uuid.UUID, step int64) (bool, error) {
	return s.steps.consume(userID, step), nil
}

func (s *fakeMFAStoreWithEncryptedSecret) IsEnabled(_ context.Context, _ uuid.UUID) (bool, error) {
	return true, nil
}
func (s *fakeMFAStoreWithEncryptedSecret) GetEnabled(_ context.Context, _ uuid.UUID) (string, []string, error) {
	return s.encSecret, nil, nil
}
func (s *fakeMFAStoreWithEncryptedSecret) HasWebAuthn(_ context.Context, _ uuid.UUID) (bool, error) {
	return false, nil
}
func (s *fakeMFAStoreWithEncryptedSecret) DisableByAdmin(_ context.Context, _, _ uuid.UUID, _ string) error {
	return nil
}
func (s *fakeMFAStoreWithEncryptedSecret) GracePeriodHours() int { return 0 }
func (s *fakeMFAStoreWithEncryptedSecret) RegenerateBackupCodes(_ context.Context, _ uuid.UUID, _ int) ([]string, error) {
	return nil, nil
}
func (s *fakeMFAStoreWithEncryptedSecret) UpdateBackupCodes(_ context.Context, _ uuid.UUID, _ []string) error {
	return nil
}
func (s *fakeMFAStoreWithEncryptedSecret) ConsumeBackupCode(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	return false, nil
}
func (s *fakeMFAStoreWithEncryptedSecret) GetLastTOTPTimestep(_ context.Context, _ uuid.UUID) (int64, error) {
	return 0, nil
}
func (s *fakeMFAStoreWithEncryptedSecret) UpdateLastTOTPTimestep(_ context.Context, _ uuid.UUID, _ int64) error {
	return nil
}

func makeMFAVerifyBody(challengeToken, code string) string {
	b, _ := json.Marshal(map[string]string{
		"challenge_token": challengeToken,
		"code":            code,
	})
	return string(b)
}
