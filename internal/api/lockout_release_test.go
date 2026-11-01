package api

import (
	"context"
	"errors"
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

// outageMFAStore is an MFA store whose writes fail while it is down, the way
// a database outage fails a code's spend after the code itself matched.
type outageMFAStore struct {
	security.MFAStore
	down atomic.Bool
}

var errMFAStoreDown = errors.New("mfa store: connection refused")

func (s *outageMFAStore) ConsumeTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (bool, error) {
	if s.down.Load() {
		return false, errMFAStoreDown
	}
	return s.MFAStore.ConsumeTOTPStep(ctx, userID, step)
}

func (s *outageMFAStore) ConsumeBackupCode(ctx context.Context, userID uuid.UUID, codeHash string) (bool, error) {
	if s.down.Load() {
		return false, errMFAStoreDown
	}
	return s.MFAStore.ConsumeBackupCode(ctx, userID, codeHash)
}

// A correct code the store cannot spend answers 503 and is not counted
// against the account, so an outage never locks out a person who typed
// correct codes through it.
func TestMFALockout_StoreOutageDoesNotCountCorrectCodes(t *testing.T) {
	for _, backend := range lockoutBackends {
		t.Run(backend+"/sign-in", func(t *testing.T) {
			t.Parallel()
			inner, totpSecret := mfaFixture(t)
			store := &outageMFAStore{MFAStore: inner}
			h := lockoutHandler(nil, store, backend)
			uid := uuid.New()
			verify := func() int {
				c, err := auth.SignChallenge(lockoutTestSecret, uid, "test@example.com", []string{"admin"}, "")
				require.NoError(t, err)
				code, err := totp.GenerateCode(totpSecret, time.Now())
				require.NoError(t, err)
				return mfaVerify(h, c, code)
			}

			store.down.Store(true)
			for i := 0; i < DefaultMFAVerifyMaxAttempts; i++ {
				require.Equal(t, http.StatusServiceUnavailable, verify(), "attempt %d during the outage", i+1)
			}
			store.down.Store(false)
			assert.Equal(t, http.StatusOK, verify(), "a correct code once the store is back")
		})
		t.Run(backend+"/admin token step-up", func(t *testing.T) {
			key, err := totp.Generate(totp.GenerateOpts{Issuer: "LyEve", AccountName: "owner@example.com"})
			require.NoError(t, err)
			enc, err := security.EncryptSecret(key.Secret(), testConfig().JWTSecret)
			require.NoError(t, err)
			store := &outageMFAStore{MFAStore: &mfaEnrolledStore{enc: enc}}
			rg := newLockoutRig(t, backend, WithMFAStore(store))
			_, session := rg.user("admin")
			stepUp := func() int {
				code, err := totp.GenerateCode(key.Secret(), time.Now())
				require.NoError(t, err)
				return rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session,
					body: adminTokenBody(map[string]any{"mfa_code": code})}).Code
			}

			store.down.Store(true)
			for i := 0; i < DefaultMFAVerifyMaxAttempts; i++ {
				require.Equal(t, http.StatusServiceUnavailable, stepUp(), "attempt %d during the outage", i+1)
			}
			store.down.Store(false)
			assert.Equal(t, http.StatusCreated, stepUp(), "a correct code once the store is back")
		})
	}
}

// A wrong authenticator code is judged before any recovery code is looked
// up, so a recovery code store that is down cannot give its attempt back.
// It answered 503 and released the attempt, which left a judged guess
// uncounted for as long as the outage lasted.
func TestMFALockout_WrongCodeCountsDuringARecoveryCodeOutage(t *testing.T) {
	for _, backend := range lockoutBackends {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			inner, totpSecret := mfaFixture(t)
			store := &outageMFAStore{MFAStore: inner}
			h := lockoutHandler(nil, store, backend)
			uid := uuid.New()
			verify := func(code string) int {
				c, err := auth.SignChallenge(lockoutTestSecret, uid, "test@example.com", []string{"admin"}, "")
				require.NoError(t, err)
				return mfaVerify(h, c, code)
			}

			store.down.Store(true)
			wrong := wrongTOTPCode(t, totpSecret)
			for i := 0; i < DefaultMFAVerifyMaxAttempts; i++ {
				require.Equal(t, http.StatusUnprocessableEntity, verify(wrong), "attempt %d during the outage", i+1)
			}
			assert.Equal(t, http.StatusTooManyRequests, verify(wrong), "five wrong codes lock the account")
		})
	}
}

// A release gives back one attempt in the window it was reserved in, and
// nothing once that window has closed and another has opened.
func TestMFALockout_ReleaseGivesBackOnlyItsOwnAttempt(t *testing.T) {
	t.Parallel()
	const window = 50 * time.Millisecond
	lockouts := map[string]MFALockoutChecker{
		"in-process": processMFALockout{mem: NewInMemoryMFALockout(5, window)},
		"shared":     NewSharedMFALockout(auth.NewFlushProtectedBackend(auth.NewMemoryBackend()), 5, window),
	}
	for name, l := range lockouts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			uid := uuid.New()
			reserve := func() (Reservation, bool) {
				r, ok, err := l.ReserveMFAAttempt(ctx, uid)
				require.NoError(t, err)
				return r, ok
			}
			left := func() int {
				n := 0
				for _, ok := reserve(); ok; _, ok = reserve() {
					n++
				}
				return n
			}

			first, ok := reserve()
			require.True(t, ok)
			_, ok = reserve()
			require.True(t, ok)
			require.NoError(t, l.ReleaseMFAAttempt(ctx, uid, first))
			assert.Equal(t, 4, left(), "one of the two attempts was given back")

			time.Sleep(2 * window)
			_, ok = reserve()
			require.True(t, ok)
			require.NoError(t, l.ReleaseMFAAttempt(ctx, uid, first))
			assert.Equal(t, 4, left(), "a release from a closed window gives back nothing in the new one")
		})
	}
}

// A password release gives back one attempt in the window it was reserved
// in, and nothing once that window has closed and another has opened.
func TestLockout_ReleaseGivesBackOnlyItsOwnAttempt(t *testing.T) {
	t.Parallel()
	const window = 50 * time.Millisecond
	lockouts := map[string]LockoutChecker{
		"in-process": NewInMemoryLockout(5, window),
		"shared":     NewSharedLockout(auth.NewFlushProtectedBackend(auth.NewMemoryBackend()), window),
	}
	for name, l := range lockouts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			uid := uuid.New()
			reserve := func() (Reservation, bool) {
				r, ok, err := l.ReserveAttempt(ctx, uid, 5)
				require.NoError(t, err)
				return r, ok
			}
			left := func() int {
				n := 0
				for _, ok := reserve(); ok; _, ok = reserve() {
					n++
				}
				return n
			}

			first, ok := reserve()
			require.True(t, ok)
			_, ok = reserve()
			require.True(t, ok)
			require.NoError(t, l.ReleaseAttempt(ctx, uid, first))
			assert.Equal(t, 4, left(), "one of the two attempts was given back")

			time.Sleep(2 * window)
			_, ok = reserve()
			require.True(t, ok)
			require.NoError(t, l.ReleaseAttempt(ctx, uid, first))
			assert.Equal(t, 4, left(), "a release from a closed window gives back nothing in the new one")
		})
	}
}

// A correct answer gives back only the attempt it reserved. The wrong
// answers before and after it stay counted, so the owner signing in does not
// hand a guesser a fresh budget: four wrong, one right and one more wrong is
// a lock.
func TestLockout_CorrectAnswerKeepsTheWrongOnesCounted(t *testing.T) {
	t.Parallel()
	for _, backend := range lockoutBackends {
		t.Run(backend+"/password", func(t *testing.T) {
			t.Parallel()
			users, _ := oneUser(t)
			h := lockoutHandler(users, nil, backend)
			login := func(password string) int {
				req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login",
					strings.NewReader(makeLoginBody("user@test.com", password)))
				req.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				h.Login(rr, req)
				return rr.Code
			}
			for i := 0; i < h.MaxFailedAttempts-1; i++ {
				require.Equal(t, http.StatusUnauthorized, login("wrong-password"), "attempt %d", i+1)
			}
			require.Equal(t, http.StatusOK, login("correct-horse-battery-staple"))
			require.Equal(t, http.StatusUnauthorized, login("wrong-password"))
			assert.Equal(t, http.StatusUnauthorized, login("correct-horse-battery-staple"),
				"five wrong passwords in the window lock the account, the correct one between them aside")
		})
		t.Run(backend+"/code", func(t *testing.T) {
			t.Parallel()
			mfa, totpSecret := mfaFixture(t)
			h := lockoutHandler(nil, mfa, backend)
			uid := uuid.New()
			verify := func(code string) int {
				c, err := auth.SignChallenge(lockoutTestSecret, uid, "test@example.com", []string{"admin"}, "")
				require.NoError(t, err)
				return mfaVerify(h, c, code)
			}
			wrong := wrongTOTPCode(t, totpSecret)
			for i := 0; i < DefaultMFAVerifyMaxAttempts-1; i++ {
				require.Equal(t, http.StatusUnprocessableEntity, verify(wrong), "attempt %d", i+1)
			}
			code, err := totp.GenerateCode(totpSecret, time.Now())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, verify(code))
			require.Equal(t, http.StatusUnprocessableEntity, verify(wrong))
			assert.Equal(t, http.StatusTooManyRequests, verify(wrong),
				"five wrong codes in the window lock the account, the correct one between them aside")
		})
	}
}
