// Package api: auth endpoint unit tests.
//
// These test input validation, error branches, and early-bailout paths
// that don't require a real database (nil stores are fine).
//
// Success-path tests (Login with real password, MFAVerify with TOTP, etc.)
// require a real database and live in auth_integration_test.go.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// Helpers

func makeLoginBody(email, password string) string {
	b, _ := json.Marshal(map[string]string{"email": email, "password": password})
	return string(b)
}

func decodeAuthResponse(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	return m
}

// LOGIN: Input validation (nil stores OK, handler rejects before touching them)

func TestLogin_BadJSON(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader("{{{"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

// LOGIN: Locked-out user

func TestLogin_LockedOut_Unit(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"locked@test.com": {ID: uid, Email: "locked@test.com", PasswordHash: hash},
		},
	}
	lockout := &fakeLockoutChecker{failCount: 5} // already locked out
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
	h.WithLockoutChecker(lockout)

	body := makeLoginBody("locked@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	// A locked account has to answer exactly as an unknown address does.
	// Only an account that exists can be locked, so a status or code of its
	// own tells a caller the address is real.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d: a lockout must not answer with its own status", rr.Code, http.StatusUnauthorized)
	}

	unknown := httptest.NewRecorder()
	unknownReq := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
		strings.NewReader(makeLoginBody("nobody@test.com", "correct-horse-battery-staple")))
	unknownReq.Header.Set("Content-Type", "application/json")
	h.Login(unknown, unknownReq)

	if got, want := rr.Body.String(), unknown.Body.String(); got != want {
		t.Errorf("locked body = %s, unknown-address body = %s: the two must be indistinguishable", got, want)
	}
}

func TestLogin_LockedOut_AfterFailures(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"user@test.com": {ID: uid, Email: "user@test.com", PasswordHash: hash},
		},
	}
	lockout := &fakeLockoutChecker{failCount: 4} // 4 failures, threshold is 5: still allowed
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
	h.WithLockoutChecker(lockout)

	body := makeLoginBody("user@test.com", "wrong-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (Unauthorized - under threshold)", rr.Code, http.StatusUnauthorized)
	}
}

// TOKEN: lockout coverage (brute-force protection)

func TestToken_LockedOut_Unit(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"token-locked@test.com": {ID: uid, Email: "token-locked@test.com", PasswordHash: hash},
		},
	}
	lockout := &fakeLockoutChecker{failCount: 5} // already locked out
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
	h.WithLockoutChecker(lockout)

	body := makeLoginBody("token-locked@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Token(rr, req)

	// A locked account answers exactly as a wrong password does, here and on
	// the admin path. Only an account that exists can be locked, so a status
	// or code of its own tells an anonymous caller the address is real, and this
	// endpoint is the more exposed of the two.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d: a lockout must not answer with its own status", rr.Code, http.StatusUnauthorized)
	}
}

func TestToken_LockedOut_AfterFailures(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"token-user@test.com": {ID: uid, Email: "token-user@test.com", PasswordHash: hash},
		},
	}
	lockout := &fakeLockoutChecker{failCount: 4} // 4 failures, threshold is 5: still allowed
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
	h.WithLockoutChecker(lockout)

	body := makeLoginBody("token-user@test.com", "wrong-password")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Token(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (Unauthorized - under threshold)", rr.Code, http.StatusUnauthorized)
	}
}

func TestToken_LockedOut_FailClosedOnCheckError(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"token-err@test.com": {ID: uid, Email: "token-err@test.com", PasswordHash: hash},
		},
	}
	lockout := &fakeLockoutChecker{failCount: 0, err: errors.New("db down")}
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)
	h.WithLockoutChecker(lockout)

	body := makeLoginBody("token-err@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Token(rr, req)

	// Fail-closed, and indistinguishable: the checker being down must refuse the
	// login without telling the caller anything it could not otherwise learn.
	// "db down" never reaches the client, and the status is the one a wrong
	// password gets.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (fail-closed, indistinguishable)", rr.Code, http.StatusUnauthorized)
	}
}

// LOGIN: Lockout triggers after N failed password attempts

type recordingLockoutChecker struct {
	failures int
}

func (c *recordingLockoutChecker) CountRecentFailures(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	return c.failures, nil
}

func (c *recordingLockoutChecker) RecordFailedAttempt(ctx context.Context, userID uuid.UUID) error {
	c.failures++
	return nil
}

func TestLogin_LockoutTriggersAfterNFailures(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"user@test.com": {ID: uid, Email: "user@test.com", PasswordHash: hash},
		},
	}
	recorder := &recordingLockoutChecker{}
	h := NewAuthHandler(users, nil, nil, "test-secret-at-least-32-bytes-long!!", 3600, false)
	h.WithLockoutChecker(recorder)
	// Override thresholds for test speed.
	h.MaxFailedAttempts = 5
	h.LockoutWindow = 15 * time.Minute

	// First 5 failed logins return 401. On the 5th, RecordFailedAttempt fires
	// after the lockout check, so the counter reaches 5 but login still returns
	// 401. The 6th attempt sees counter=5 and blocks.
	for i := 0; i < 5; i++ {
		body := makeLoginBody("user@test.com", "wrong-password")
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Login(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("attempt %d: status = %d, want %d (Unauthorized - under threshold)", i+1, rr.Code, http.StatusUnauthorized)
		}
	}

	// 6th attempt: locked out.
	body := makeLoginBody("user@test.com", "wrong-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)

	// The lockout is in force, and says so to nobody: the answer matches every
	// other rejection, so failing repeatedly against an address reveals
	// nothing about whether it exists.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("6th attempt: status = %d, want %d: a lockout must look like any other rejection", rr.Code, http.StatusUnauthorized)
	}

	if recorder.failures != 5 {
		t.Errorf("recorded failures = %d, want 5", recorder.failures)
	}
}

// LOGIN: Expired user

func TestLogin_ExpiredUser_Unit(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")
	expired := time.Now().Add(-1 * time.Hour) // expired 1 hour ago

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"expired@test.com": {ID: uid, Email: "expired@test.com", PasswordHash: hash, ExpiresAt: &expired},
		},
	}
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)

	body := makeLoginBody("expired@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (Unauthorized - expired account)", rr.Code, http.StatusUnauthorized)
	}
}

func TestLogin_DisabledUser_Unit(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"disabled@test.com": {ID: uid, Email: "disabled@test.com", PasswordHash: hash, Disabled: true},
		},
	}
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)

	body := makeLoginBody("disabled@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (Unauthorized - disabled account)", rr.Code, http.StatusUnauthorized)
	}
}

// After successful password verification, disabled and expired accounts must
// return the same generic i18n code (CodeUnauthorized) so an attacker with
// valid credentials cannot distinguish account states.
func TestLogin_AccountState_NoEnumeration(t *testing.T) {
	t.Parallel()

	uid1 := uuid.New()
	uid2 := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")
	expired := time.Now().Add(-1 * time.Hour)

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"disabled@test.com": {ID: uid1, Email: "disabled@test.com", PasswordHash: hash, Disabled: true},
			"expired@test.com":  {ID: uid2, Email: "expired@test.com", PasswordHash: hash, ExpiresAt: &expired},
		},
	}
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)

	var body string
	var req *http.Request
	var rr *httptest.ResponseRecorder

	body = makeLoginBody("disabled@test.com", "correct-horse-battery-staple")
	req = httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("disabled: status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	disabledResp := decodeAuthResponse(t, rr)
	disabledCode, _ := disabledResp["code"].(string)
	// A caller holding the correct password would otherwise learn the account
	// exists and is locked, so this has to match the credentials-failure code.
	if disabledCode != "AUTH_INVALID_CREDENTIALS" {
		t.Errorf("disabled account: code = %q, want %q", disabledCode, "AUTH_INVALID_CREDENTIALS")
	}

	body = makeLoginBody("expired@test.com", "correct-horse-battery-staple")
	req = httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.Login(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expired: status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	expiredResp := decodeAuthResponse(t, rr)
	expiredCode, _ := expiredResp["code"].(string)
	if expiredCode != "AUTH_INVALID_CREDENTIALS" {
		t.Errorf("expired account: code = %q, want %q", expiredCode, "AUTH_INVALID_CREDENTIALS")
	}
	if disabledCode != expiredCode {
		t.Errorf("disabled code %q and expired code %q differ, which separates the two states", disabledCode, expiredCode)
	}
}

// LOGIN: Enumeration prevention

func TestLogin_EnumerationResistance_Unit(t *testing.T) {
	t.Parallel()

	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	knownUser := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"known@test.com": {ID: uuid.New(), Email: "known@test.com", PasswordHash: hash},
		},
	}
	hKnown := NewAuthHandler(knownUser, nil, nil, "secret", 3600, false)

	unknownUser := &fakeUserStore{
		byEmail: map[string]*domain.User{},
	}
	hUnknown := NewAuthHandler(unknownUser, nil, nil, "secret", 3600, false)

	testCases := []struct {
		name  string
		h     *AuthHandler
		email string
	}{
		{"known user, wrong password", hKnown, "known@test.com"},
		{"unknown user", hUnknown, "noone@test.com"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body := makeLoginBody(tc.email, "wrong-password")
			req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			tc.h.Login(rr, req)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
			}
		})
	}
}

// Unknown-email and known-email+wrong-password paths must be
// indistinguishable in timing (both run bcrypt). Runs each path many
// times, computes the median, and asserts the difference is within
// tolerance.
func TestLogin_EnumerationTiming_Unit(t *testing.T) {
	t.Parallel()

	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	knownUser := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"known@test.com": {ID: uuid.New(), Email: "known@test.com", PasswordHash: hash},
		},
	}
	hKnown := NewAuthHandler(knownUser, nil, nil, "secret", 3600, false)
	hKnown.WithLockoutChecker(&fakeLockoutChecker{failCount: 0}) // disable lockout for timing test

	unknownUser := &fakeUserStore{
		byEmail: map[string]*domain.User{},
	}
	hUnknown := NewAuthHandler(unknownUser, nil, nil, "secret", 3600, false)

	const iterations = 20
	measure := func(h *AuthHandler, email string) []time.Duration {
		durs := make([]time.Duration, iterations)
		for i := 0; i < iterations; i++ {
			body := makeLoginBody(email, "wrong-password")
			req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			start := time.Now()
			h.Login(rr, req)
			durs[i] = time.Since(start)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s status = %d, want %d", email, rr.Code, http.StatusUnauthorized)
			}
		}
		return durs
	}

	knownDurs := measure(hKnown, "known@test.com")
	unknownDurs := measure(hUnknown, "noone@test.com")

	sort.Slice(knownDurs, func(i, j int) bool { return knownDurs[i] < knownDurs[j] })
	sort.Slice(unknownDurs, func(i, j int) bool { return unknownDurs[i] < unknownDurs[j] })

	// Use medians to filter out cold-cache outliers.
	medianKnown := knownDurs[iterations/2]
	medianUnknown := unknownDurs[iterations/2]

	// Both paths run bcrypt; expected times are comparable. Allow unknown
	// to be somewhat slower (dummy hash compare is a cheap constant fetch +
	// bcrypt call, not a real DB lookup). No order-of-magnitude difference
	// that would let an attacker distinguish via wall-clock timing.
	ratio := float64(medianUnknown) / float64(medianKnown)
	if ratio < 0.1 || ratio > 10.0 {
		t.Errorf("timing ratio unknown/known = %.2f, want [0.1, 10.0]; median known=%v, median unknown=%v",
			ratio, medianKnown, medianUnknown)
	}

	knownReq := makeLoginBody("known@test.com", "wrong-password")
	knownRR := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(knownReq))
	knownRR.Header.Set("Content-Type", "application/json")
	knownRespRR := httptest.NewRecorder()
	hKnown.Login(knownRespRR, knownRR)

	unknownReq := makeLoginBody("noone@test.com", "wrong-password")
	unknownRR := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(unknownReq))
	unknownRR.Header.Set("Content-Type", "application/json")
	unknownRespRR := httptest.NewRecorder()
	hUnknown.Login(unknownRespRR, unknownRR)

	knownResp := decodeAuthResponse(t, knownRespRR)
	unknownResp := decodeAuthResponse(t, unknownRespRR)

	if knownResp["error"] != unknownResp["error"] {
		t.Errorf("error messages differ: known=%q, unknown=%q", knownResp["error"], unknownResp["error"])
	}
}

// LOGIN: MFA fail-closed

func TestLogin_MFAFailClosed_Unit(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"user@test.com": {ID: uid, Email: "user@test.com", PasswordHash: hash},
		},
	}
	// MFA store that errors on IsEnabled: simulates DB failure.
	mfa := &failingMFAStore{}

	h := NewAuthHandler(users, mfa, nil, "secret", 3600, false)

	body := makeLoginBody("user@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	// Must fail closed: a store error never issues a session.
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (Unauthorized - MFA store error must fail closed)", rr.Code, http.StatusUnauthorized)
	}
}

func TestLogin_MFAFailOpen_Logged(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"user@test.com": {ID: uid, Email: "user@test.com", PasswordHash: hash},
		},
	}
	mfa := &fakeMFAStore{}

	h := NewAuthHandler(users, mfa, nil, "secret", 3600, false)

	body := makeLoginBody("user@test.com", "correct-horse-battery-staple")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (OK - MFA not enabled, no error)", rr.Code, http.StatusOK)
	}
}

// Fake stores for unit tests

type fakeUserStore struct {
	byEmail map[string]*domain.User
	mu      sync.RWMutex
}

func (s *fakeUserStore) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byEmail[email]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}

func (s *fakeUserStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.byEmail {
		if u.ID == id {
			return u, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *fakeUserStore) Count(ctx context.Context) (int64, error) {
	return int64(len(s.byEmail)), nil
}

func (s *fakeUserStore) Create(ctx context.Context, email, passwordHash string, roles []string, tenantID string) (*domain.User, error) {
	return nil, errors.New("not implemented")
}

func (s *fakeUserStore) CreateFirstAdmin(ctx context.Context, email, passwordHash, tenantID string) (*domain.User, error) {
	return nil, errors.New("not implemented")
}

func (s *fakeUserStore) GetTokenVersion(ctx context.Context, id uuid.UUID) (int, error) {
	return 0, nil
}

func (s *fakeUserStore) BumpTokenVersion(ctx context.Context, id uuid.UUID) error {
	return nil
}

type fakeLockoutChecker struct {
	failCount int
	err       error
}

func (c *fakeLockoutChecker) CountRecentFailures(ctx context.Context, userID uuid.UUID, since time.Time) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	return c.failCount, nil
}

func (c *fakeLockoutChecker) RecordFailedAttempt(ctx context.Context, userID uuid.UUID) error {
	return nil
}

type failingMFAStore struct{}

func (s *failingMFAStore) ConsumeTOTPStep(context.Context, uuid.UUID, int64) (bool, error) {
	return false, errors.New("mfa store unavailable")
}

func (s *failingMFAStore) IsEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, errors.New("mfa store unavailable")
}
func (s *failingMFAStore) GetEnabled(ctx context.Context, userID uuid.UUID) (string, []string, error) {
	return "", nil, errors.New("mfa store unavailable")
}
func (s *failingMFAStore) HasWebAuthn(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}
func (s *failingMFAStore) DisableByAdmin(ctx context.Context, targetUserID, adminUserID uuid.UUID, reason string) error {
	return nil
}
func (s *failingMFAStore) GracePeriodHours() int { return 0 }
func (s *failingMFAStore) RegenerateBackupCodes(ctx context.Context, userID uuid.UUID, n int) ([]string, error) {
	return nil, nil
}
func (s *failingMFAStore) UpdateBackupCodes(ctx context.Context, userID uuid.UUID, hashes []string) error {
	return nil
}

// dbErrorUserStore returns an error on every method: used to test DB error
// code paths return 503 Service Unavailable instead of 500.
type dbErrorUserStore struct{}

func (s *dbErrorUserStore) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	return nil, errors.New("db unavailable")
}
func (s *dbErrorUserStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return nil, errors.New("db unavailable")
}
func (s *dbErrorUserStore) Count(ctx context.Context) (int64, error) {
	return 0, errors.New("db unavailable")
}
func (s *dbErrorUserStore) Create(ctx context.Context, email, passwordHash string, roles []string, tenantID string) (*domain.User, error) {
	return nil, errors.New("db unavailable")
}
func (s *dbErrorUserStore) CreateFirstAdmin(ctx context.Context, email, passwordHash, tenantID string) (*domain.User, error) {
	return nil, errors.New("db unavailable")
}

func (s *dbErrorUserStore) GetTokenVersion(ctx context.Context, id uuid.UUID) (int, error) {
	return 0, errors.New("db unavailable")
}
func (s *dbErrorUserStore) BumpTokenVersion(ctx context.Context, id uuid.UUID) error {
	return errors.New("db unavailable")
}

// DB ERROR PATHS: verify 503 ServiceUnavailable instead of 500

func TestSetupStatus_DBError_503(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/setup-status", nil)
	rr := httptest.NewRecorder()

	h.SetupStatus(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("SetupStatus DB error: status = %d, want %d (503 ServiceUnavailable)",
			rr.Code, http.StatusServiceUnavailable)
	}
}

func TestSetup_DBError_503(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
	h.WithSetupToken(NewSetupTokenFromEnv("operator-setup-token-0123"))
	body := `{"email":"admin@test.com","password":"CorrectHorseBatteryStaple1!","setup_token":"operator-setup-token-0123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Setup(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Setup DB error: status = %d, want %d (503 ServiceUnavailable)",
			rr.Code, http.StatusServiceUnavailable)
	}
}

func TestLogin_DBError_503(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
	body := makeLoginBody("any@test.com", "any-password")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Login(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Login DB error: status = %d, want %d (503 ServiceUnavailable)",
			rr.Code, http.StatusServiceUnavailable)
	}
}

func TestToken_DBError_503(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
	body := `{"email":"any@test.com","password":"any-password"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Token(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Token DB error: status = %d, want %d (503 ServiceUnavailable)",
			rr.Code, http.StatusServiceUnavailable)
	}
}

// Issue a valid refresh token first, then trigger a DB error on the
// GetByID call during rotation.
func TestRefresh_DBError_503(t *testing.T) {
	t.Parallel()

	rtStore := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
	issued, err := rtStore.Issue(context.Background(), uuid.New().String(), time.Hour)
	if err != nil {
		t.Fatalf("Issue refresh token: %v", err)
	}

	h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
	h.WithRefreshTokenStore(rtStore, time.Hour)

	body := fmt.Sprintf(`{"refresh_token":"%s"}`, issued.RefreshToken)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Refresh(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("Refresh DB error: status = %d, want %d (503 ServiceUnavailable)",
			rr.Code, http.StatusServiceUnavailable)
	}
}

// MFA VERIFY: Input validation (nil stores OK)

func TestAuthEndpoint_MFAVerify_BadJSON(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "secret", 3600, false)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.MFAVerify(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestAuthEndpoint_MFAVerify_MissingFields(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "secret", 3600, false)

	tests := []struct {
		name string
		body string
	}{
		{"empty both", `{"challenge_token":"","code":""}`},
		{"missing code", `{"challenge_token":"some-token"}`},
		{"missing challenge_token", `{"code":"123456"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.MFAVerify(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want %d", tt.name, rr.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestAuthEndpoint_MFAVerify_BadChallengeToken(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, &stubMFAStore{}, nil, "test-secret-at-least-32-bytes-long!!", 3600, false)

	body, _ := json.Marshal(map[string]string{"challenge_token": "not-a-valid-jwt", "code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.MFAVerify(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

func TestAuthEndpoint_MFAVerify_FullSessionTokenRejected(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()

	// Need an MFAStore so the handler reaches token parsing. A full session
	// token (MFAPending=false) must be rejected before any store calls.
	mfaStore := &fakeMFAStore{}

	h := NewAuthHandler(nil, mfaStore, nil, secret, 3600, false)

	// Create a full session token (typ=session, NOT pending MFA).
	fullToken, err := auth.Sign(secret, 3600, userID, "full-token@test.com", []string{"editor"}, "", 1)
	if err != nil {
		t.Fatalf("sign full token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"challenge_token": fullToken, "code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.MFAVerify(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (full session token must not be accepted for MFA)", rr.Code, http.StatusUnauthorized)
	}
}

// dekMFAStore serves a TOTP secret that was encrypted with a per-tenant DEK,
// the way an MFA store seals it once ENCRYPTION_KEY is configured.
type dekMFAStore struct {
	fakeMFAStore
	encSecret string
}

func (s *dekMFAStore) GetEnabled(context.Context, uuid.UUID) (string, []string, error) {
	return s.encSecret, nil, nil
}

// A tenantless TOTP secret is sealed under the default key id, and MFAVerify
// must derive that key to read it.
func TestAuthEndpoint_MFAVerify_TenantlessSecretDecryptsUnderDefaultDEK(t *testing.T) {
	t.Parallel()

	const jwtSecret = "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()

	ks, err := encryption.NewKeyStore("mfa-verify-master-key-at-least-32b!!")
	if err != nil {
		t.Fatalf("new key store: %v", err)
	}
	totpSecret, _, err := auth.GenerateTOTP("tenantless@test.com", nil)
	if err != nil {
		t.Fatalf("generate totp secret: %v", err)
	}
	encSecret, err := ks.EncryptPlaintext(mfaDefaultTenantKeyID, []byte(totpSecret))
	if err != nil {
		t.Fatalf("encrypt totp secret: %v", err)
	}

	h := NewAuthHandler(nil, &dekMFAStore{encSecret: encSecret}, nil, jwtSecret, 3600, false)
	h.WithKeyStore(ks)

	challenge, err := auth.SignChallenge(jwtSecret, userID, "tenantless@test.com", []string{"super_admin"}, "")
	if err != nil {
		t.Fatalf("sign challenge: %v", err)
	}
	code, err := totp.GenerateCode(totpSecret, time.Now())
	if err != nil {
		t.Fatalf("generate totp code: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"challenge_token": challenge, "code": code})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.MFAVerify(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if tok, _ := resp["token"].(string); tok == "" {
		t.Error("a completed MFA challenge must return a full session token")
	}
}

// LOGOUT: Always works, no store needed

func TestAuthEndpoint_Logout_ClearsCookie(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", nil)
	rr := httptest.NewRecorder()

	h.Logout(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	cookies := rr.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == security.SessionCookieNameInsecure {
			found = true
			if c.Value != "" {
				t.Error("clearing cookie should have empty value")
			}
			if c.MaxAge != -1 {
				t.Errorf("clearing cookie MaxAge = %d, want -1", c.MaxAge)
			}
		}
	}
	if !found {
		t.Error("should set clearing cookie on logout")
	}
}

// ME: Authorized / Unauthorized

func TestMe_Authenticated(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)

	claims := makeClaims(uid, "me@test.com", []string{"editor"})
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/me", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
	rr := httptest.NewRecorder()

	h.Me(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	resp := decodeAuthResponse(t, rr)
	if resp["id"] != uid.String() {
		t.Errorf("id = %v, want %v", resp["id"], uid.String())
	}
	if resp["email"] != "me@test.com" {
		t.Errorf("email = %v, want me@test.com", resp["email"])
	}
}

func TestMe_Unauthenticated(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/me", nil)
	rr := httptest.NewRecorder()

	h.Me(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
}

// REFRESH: Not enabled when no refresh store

func TestRefresh_NotEnabled(t *testing.T) {
	t.Parallel()

	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(`{"refresh_token":"any"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Refresh(rr, req)

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusNotImplemented)
	}
}

// REFRESH: Disabled user rejection

func TestRefresh_DisabledUser(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	secret := "test-secret-at-least-32-bytes-long!!"
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"disabled@test.com": {ID: uid, Email: "disabled@test.com", PasswordHash: hash, Roles: []string{"editor"}, Disabled: true},
		},
	}

	rtStore := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")

	h := NewAuthHandler(users, nil, nil, secret, 3600, false)
	h.WithRefreshTokenStore(rtStore, 15*time.Minute)

	// Issue a refresh token first (simulating a prior login).
	issued, err := rtStore.Issue(context.Background(), uid.String(), 15*time.Minute)
	if err != nil {
		t.Fatalf("issue refresh token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"refresh_token": issued.RefreshToken})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Refresh(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (disabled user must be rejected)", rr.Code, http.StatusUnauthorized)
	}

	resp := decodeAuthResponse(t, rr)
	if resp["code"] != "UNAUTHORIZED" {
		t.Errorf("error code = %v, want UNAUTHORIZED (no state enumeration)", resp["code"])
	}
}

// REFRESH: Expired user rejection

func TestRefresh_ExpiredUser(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	secret := "test-secret-at-least-32-bytes-long!!"
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")
	expired := time.Now().Add(-1 * time.Hour) // expired 1 hour ago

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"expired@test.com": {ID: uid, Email: "expired@test.com", PasswordHash: hash, Roles: []string{"editor"}, ExpiresAt: &expired},
		},
	}

	rtStore := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")

	h := NewAuthHandler(users, nil, nil, secret, 3600, false)
	h.WithRefreshTokenStore(rtStore, 15*time.Minute)

	issued, err := rtStore.Issue(context.Background(), uid.String(), 15*time.Minute)
	if err != nil {
		t.Fatalf("issue refresh token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"refresh_token": issued.RefreshToken})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Refresh(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (expired user must be rejected)", rr.Code, http.StatusUnauthorized)
	}

	resp := decodeAuthResponse(t, rr)
	if resp["code"] != "UNAUTHORIZED" {
		t.Errorf("error code = %v, want UNAUTHORIZED (no state enumeration)", resp["code"])
	}
}

// REFRESH (Role downgrade): uses fresh roles from DB, not stale claims

func TestRefresh_RoleDowngrade_UsesFreshRoles(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	secret := "test-secret-at-least-32-bytes-long!!"
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	users := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"role-test@test.com": {
				ID: uid, Email: "role-test@test.com",
				PasswordHash: hash,
				Roles:        []string{"viewer"}, // freshly downgraded role
			},
		},
	}

	rtStore := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")

	h := NewAuthHandler(users, nil, nil, secret, 3600, false)
	h.WithRefreshTokenStore(rtStore, 15*time.Minute)

	issued, err := rtStore.Issue(context.Background(), uid.String(), 15*time.Minute)
	if err != nil {
		t.Fatalf("issue refresh token: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"refresh_token": issued.RefreshToken})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Refresh(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", rr.Code, http.StatusOK, rr.Body.String())
	}

	resp := decodeAuthResponse(t, rr)
	tokenStr, ok := resp["token"].(string)
	if !ok || tokenStr == "" {
		t.Fatal("refresh response missing access token")
	}

	// Parse the issued access token and verify it has the fresh "viewer" role.
	claims, err := auth.Parse(secret, tokenStr)
	if err != nil {
		t.Fatalf("parse issued access token: %v", err)
	}

	if len(claims.Roles) != 1 || claims.Roles[0] != "viewer" {
		t.Errorf("access token roles = %v, want [viewer] (must use fresh DB roles, not stale claims)", claims.Roles)
	}
}

// PASSWORD HASH ALGO: Constructor tests

func TestNewAuthHandler_DefaultsBcrypt(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false)
	if h.passwordHashAlgo != "bcrypt" {
		t.Errorf("algo = %s, want bcrypt", h.passwordHashAlgo)
	}
}

func TestNewAuthHandler_ExplicitArgon2ID(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false, "argon2id")
	if h.passwordHashAlgo != "argon2id" {
		t.Errorf("algo = %s, want argon2id", h.passwordHashAlgo)
	}
}

func TestNewAuthHandler_EmptyAlgoDefaultsBcrypt(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(nil, nil, nil, "secret", 3600, false, "")
	if h.passwordHashAlgo != "bcrypt" {
		t.Errorf("algo = %s, want bcrypt", h.passwordHashAlgo)
	}
}

// fakeMFAStore: minimal security.MFAStore for MFAVerify input validation tests

type fakeMFAStore struct {
	steps totpStepLog
}

func (s *fakeMFAStore) ConsumeTOTPStep(_ context.Context, userID uuid.UUID, step int64) (bool, error) {
	return s.steps.consume(userID, step), nil
}

func (s *fakeMFAStore) IsEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}
func (s *fakeMFAStore) GetEnabled(ctx context.Context, userID uuid.UUID) (string, []string, error) {
	return "", nil, nil
}
func (s *fakeMFAStore) HasWebAuthn(ctx context.Context, userID uuid.UUID) (bool, error) {
	return false, nil
}
func (s *fakeMFAStore) DisableByAdmin(ctx context.Context, targetUserID, adminUserID uuid.UUID, reason string) error {
	return nil
}
func (s *fakeMFAStore) GracePeriodHours() int {
	return 0
}
func (s *fakeMFAStore) RegenerateBackupCodes(ctx context.Context, userID uuid.UUID, n int) ([]string, error) {
	return nil, nil
}
func (s *fakeMFAStore) UpdateBackupCodes(ctx context.Context, userID uuid.UUID, hashes []string) error {
	return nil
}

// INTEGRATION: jwtAuth rejects non-session token types (typ enforcement)

// An MFA challenge token (typ=challenge) presented as Bearer to a
// protected endpoint must be rejected with 401.
func TestJWTAuth_RejectsChallengeToken(t *testing.T) {
	t.Parallel()

	secret := "test-secret-32-bytes-minimum-len!!"
	userID := uuid.New()

	challengeToken, err := auth.SignChallenge(secret, userID, "challenge@test.com", []string{"editor"}, "")
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r_ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"ok"`))
	}))
	r.Get("/api/admin/test", func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
	req.Header.Set("Authorization", "Bearer "+challengeToken)
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (challenge token must not authenticate to /api/admin endpoints)",
			rr.Code, http.StatusUnauthorized)
	}
}

// A token typed magiclink (the shape a passwordless login plugin would mint
// for its link) presented as Bearer to a protected endpoint must be rejected
// with 401: the type is refused whether or not such a plugin is compiled in.
func TestJWTAuth_RejectsMagicLinkToken(t *testing.T) {
	t.Parallel()

	secret := "test-secret-32-bytes-minimum-len!!"

	// Mint a link token in that shape (HMAC-SHA256, typ=magiclink).
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"email": "someone@example.com",
		"nonce": uuid.New().String(),
		"iat":   now.Unix(),
		"exp":   now.Add(10 * time.Minute).Unix(),
		"jti":   uuid.New().String(),
		"aud":   "passwordless",
		"sub":   "passwordless:someone@example.com",
		"iss":   "lyeve-cms",
		"typ":   "magiclink",
	})
	magicToken, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("Sign magic token: %v", err)
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r_ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"ok"`))
	}))
	r.Get("/api/admin/test", func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
	req.Header.Set("Authorization", "Bearer "+magicToken)
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (magic-link token must not authenticate to /api/admin endpoints)",
			rr.Code, http.StatusUnauthorized)
	}
}

// A session token (typ=session) passes through jwtAuth + requireAuth.
func TestJWTAuth_AcceptsSessionToken(t *testing.T) {
	t.Parallel()

	secret := "test-secret-32-bytes-minimum-len!!"
	userID := uuid.New()

	sessionToken, err := auth.Sign(secret, 3600, userID, "session@test.com", []string{"editor"}, "", 1)
	if err != nil {
		t.Fatalf("Sign session: %v", err)
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r_ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"ok"`))
	}))
	r.Get("/api/admin/test", func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
	req.Header.Set("Authorization", "Bearer "+sessionToken)
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (session token with typ=session must pass auth)",
			rr.Code, http.StatusOK)
	}
}

// Tokens without a typ claim are rejected by requireAuth after jwtAuth
// passes them through.
func TestJWTAuth_RejectsNoTypeClaim(t *testing.T) {
	t.Parallel()

	secret := "test-secret-32-bytes-minimum-len!!"

	// Create a token without any typ claim using low-level jwt.
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   uuid.New().String(),
		"email": "notyp@test.com",
		"roles": []string{"editor"},
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	noTypToken, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("Sign no-typ token: %v", err)
	}

	r := chi.NewRouter()
	r.Use(jwtAuth([]string{secret}, false))
	h := requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r_ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"ok"`))
	}))
	r.Get("/api/admin/test", func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) })

	req := httptest.NewRequest(http.MethodGet, "/api/admin/test", nil)
	req.Header.Set("Authorization", "Bearer "+noTypToken)
	rr := httptest.NewRecorder()

	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (token without typ claim must not authenticate)",
			rr.Code, http.StatusUnauthorized)
	}
}

// TOKEN: Enumeration-timing mitigation

// Unknown-email and known-email+wrong-password paths in the Token handler
// must be indistinguishable in timing (both run a bcrypt compare),
// preventing user-enumeration oracles.
// Not parallel: this is a wall-clock measurement, and a loaded suite skews it.
//
// The band is wide on purpose. What this can see is whether the unknown-email
// path runs a bcrypt compare at all: skipping it answers in microseconds
// against roughly 60ms, a difference of three orders of magnitude. A cost
// factor that drifts, which shows up as a clean 2x, is not something a wall
// clock can separate from scheduler noise, so it is pinned exactly in
// internal/auth by TestDummyBcryptHash_CostMatchesTheCostRealHashesUse.
func TestToken_EnumerationTiming_Unit(t *testing.T) {
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")

	knownUser := &fakeUserStore{
		byEmail: map[string]*domain.User{
			"known@test.com": {ID: uuid.New(), Email: "known@test.com", PasswordHash: hash},
		},
	}
	hKnown := NewAuthHandler(knownUser, nil, nil, "secret", 3600, false)

	unknownUser := &fakeUserStore{
		byEmail: map[string]*domain.User{},
	}
	hUnknown := NewAuthHandler(unknownUser, nil, nil, "secret", 3600, false)

	const iterations = 20
	measure := func(h *AuthHandler, email string) []time.Duration {
		durs := make([]time.Duration, iterations)
		for i := 0; i < iterations; i++ {
			body := fmt.Sprintf(`{"email":"%s","password":"wrong-password"}`, email)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			start := time.Now()
			h.Token(rr, req)
			durs[i] = time.Since(start)

			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s status = %d, want %d", email, rr.Code, http.StatusUnauthorized)
			}
		}
		return durs
	}

	knownDurs := measure(hKnown, "known@test.com")
	unknownDurs := measure(hUnknown, "noone@test.com")

	sort.Slice(knownDurs, func(i, j int) bool { return knownDurs[i] < knownDurs[j] })
	sort.Slice(unknownDurs, func(i, j int) bool { return unknownDurs[i] < unknownDurs[j] })

	medianKnown := knownDurs[iterations/2]
	medianUnknown := unknownDurs[iterations/2]

	ratio := float64(medianUnknown) / float64(medianKnown)
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("timing ratio unknown/known = %.2f, want [0.5, 2.0]; median known=%v, median unknown=%v",
			ratio, medianKnown, medianUnknown)
	}

	knownReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token",
		strings.NewReader(`{"email":"known@test.com","password":"wrong-password"}`))
	knownReq.Header.Set("Content-Type", "application/json")
	knownRR := httptest.NewRecorder()
	hKnown.Token(knownRR, knownReq)

	unknownReq := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token",
		strings.NewReader(`{"email":"noone@test.com","password":"wrong-password"}`))
	unknownReq.Header.Set("Content-Type", "application/json")
	unknownRR := httptest.NewRecorder()
	hUnknown.Token(unknownRR, unknownReq)

	knownResp := decodeAuthResponse(t, knownRR)
	unknownResp := decodeAuthResponse(t, unknownRR)

	if knownResp["error"] != unknownResp["error"] {
		t.Errorf("error messages differ: known=%q, unknown=%q", knownResp["error"], unknownResp["error"])
	}
}

// TestRefresh_CacheOutageIs503NotUnauthorized pins the difference a client acts
// on. A 401 means the session is gone and the client discards it. During a
// cache outage nothing is known about the token either way, so answering 401
// would log every user out permanently over a blip that was going to clear.
func TestRefresh_CacheOutageIs503NotUnauthorized(t *testing.T) {
	t.Parallel()

	backend := &outageBackend{inner: auth.NewMemoryBackend()}
	rtStore := auth.NewRefreshTokenStore(backend, "cms-test")
	issued, err := rtStore.Issue(context.Background(), uuid.New().String(), time.Hour)
	if err != nil {
		t.Fatalf("Issue refresh token: %v", err)
	}

	backend.down = true

	h := NewAuthHandler(&dbErrorUserStore{}, nil, nil, "secret", 3600, false)
	h.WithRefreshTokenStore(rtStore, time.Hour)

	body := fmt.Sprintf(`{"refresh_token":"%s"}`, issued.RefreshToken)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.Refresh(rr, req)

	if rr.Code == http.StatusUnauthorized {
		t.Fatal("Refresh answered 401 during a cache outage; the client would discard a live session")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusServiceUnavailable)
	}
}

// outageBackend serves normally until down is set, then fails like an
// unreachable cache.
type outageBackend struct {
	inner core.CacheBackend
	down  bool
}

var errOutage = errors.New("dial tcp: connection refused")

func (o *outageBackend) Get(ctx context.Context, key string) ([]byte, error) {
	if o.down {
		return nil, errOutage
	}
	return o.inner.Get(ctx, key)
}

func (o *outageBackend) Set(ctx context.Context, key string, v []byte, ttl time.Duration) error {
	if o.down {
		return errOutage
	}
	return o.inner.Set(ctx, key, v, ttl)
}

func (o *outageBackend) Delete(ctx context.Context, key string) error {
	if o.down {
		return errOutage
	}
	return o.inner.Delete(ctx, key)
}

func (o *outageBackend) Flush(ctx context.Context) error {
	if o.down {
		return errOutage
	}
	return o.inner.Flush(ctx)
}

// countingUserStore reports whether a lookup reached the store at all.
type countingUserStore struct {
	fakeUserStore
	lookups int
}

func (s *countingUserStore) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	s.lookups++
	return nil, domain.ErrNotFound
}

// An address carrying a control character cannot match a stored row, so it is
// refused before the lookup, with the dummy compare and the answer any unknown
// email gets.
func TestLogin_ControlCharacterEmailIsRejectedWithoutALookup(t *testing.T) {
	t.Parallel()

	for _, email := range []string{"admin\x00@lyeve.test", "admin\r\n@lyeve.test", "admin\x7f@lyeve.test"} {
		users := &countingUserStore{}
		h := NewAuthHandler(users, nil, nil, "secret", 3600, false)

		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
			strings.NewReader(makeLoginBody(email, "whatever")))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Login(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("Login(%q): status = %d, want 401", email, rr.Code)
		}
		if users.lookups != 0 {
			t.Errorf("Login(%q): reached the user store %d time(s), want 0", email, users.lookups)
		}
	}
}

func TestToken_ControlCharacterEmailIsRejectedWithoutALookup(t *testing.T) {
	t.Parallel()

	users := &countingUserStore{}
	h := NewAuthHandler(users, nil, nil, "secret", 3600, false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token",
		strings.NewReader(makeLoginBody("admin\x00@lyeve.test", "whatever")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Token(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
	if users.lookups != 0 {
		t.Errorf("reached the user store %d time(s), want 0", users.lookups)
	}
}

// REFRESH: a family issued before the token version moved

// Logout revokes only the family it is shown, and refresh signs with the live
// token_version, so without a version check another device's family would
// mint a session that the logout, disable or password set meant to end.
func TestRefresh_FamilyIssuedUnderAnOlderTokenVersion(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	secret := "test-secret-at-least-32-bytes-long!!"
	hash, _ := auth.HashPassword("bcrypt", "correct-horse-battery-staple")
	user := &domain.User{ID: uid, Email: "devices@test.com", PasswordHash: hash, Roles: []string{"editor"}, TokenVersion: 3}
	users := &fakeUserStore{byEmail: map[string]*domain.User{"devices@test.com": user}}

	rtStore := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
	h := NewAuthHandler(users, nil, nil, secret, 3600, false)
	h.WithRefreshTokenStore(rtStore, 15*time.Minute)

	refresh := func(token string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"refresh_token": token})
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.Refresh(rr, req)
		return rr
	}

	otherDevice, err := rtStore.IssueForSession(context.Background(), uid.String(), "", 3, 15*time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	user.TokenVersion = 4 // a logout elsewhere, a disable lifted, a password set

	if rr := refresh(otherDevice.RefreshToken); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stale family: status = %d, want 401", rr.Code)
	}
	if _, err := rtStore.Rotate(context.Background(), otherDevice.RefreshToken, 15*time.Minute); err == nil {
		t.Error("a refused family must be revoked, not left to try again")
	}

	current, err := rtStore.IssueForSession(context.Background(), uid.String(), "", 4, 15*time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if rr := refresh(current.RefreshToken); rr.Code != http.StatusOK {
		t.Fatalf("current family: status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	legacy, err := rtStore.Issue(context.Background(), uid.String(), 15*time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if rr := refresh(legacy.RefreshToken); rr.Code != http.StatusUnauthorized {
		t.Fatalf("family with no version: status = %d, want 401", rr.Code)
	}
}
