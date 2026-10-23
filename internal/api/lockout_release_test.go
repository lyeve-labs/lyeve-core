package api

import (
	"context"
	"errors"
	"net/http"
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
