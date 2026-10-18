package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// rendezvous holds n callers until all have arrived, then lets them through
// one at a time in ascending key order. It gives up waiting after five
// seconds, so a caller that never arrives fails the test instead of hanging it.
type rendezvous struct {
	n       int
	mu      sync.Mutex
	cond    *sync.Cond
	arrived int
	pending []int64
	expired bool
}

func newRendezvous(n int) *rendezvous {
	r := &rendezvous{n: n}
	r.cond = sync.NewCond(&r.mu)
	time.AfterFunc(5*time.Second, func() {
		r.mu.Lock()
		r.expired = true
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	return r
}

func (r *rendezvous) await(key int64, fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.arrived++
	r.pending = append(r.pending, key)
	r.cond.Broadcast()
	for !r.expired && (r.arrived < r.n || slices.Min(r.pending) < key) {
		r.cond.Wait()
	}
	if fn != nil {
		fn()
	}
	r.pending = slices.Delete(r.pending, slices.Index(r.pending, key), slices.Index(r.pending, key)+1)
	r.cond.Broadcast()
}

// racingMFAStore holds two concurrent completions together after both have
// read the user's factors, the widest window a real store leaves open.
// Authenticator steps are consumed in ascending order, so both of two codes
// for adjacent steps are accepted. UpdateBackupCodes overwrites the whole set
// the way the MFA plugin's does.
type racingMFAStore struct {
	fakeMFAStore
	encSecret string
	mu        sync.Mutex
	unspent   []string
	reads     *rendezvous
	totpSteps *rendezvous
}

func newRacingMFAStore(encSecret string, codes ...string) *racingMFAStore {
	s := &racingMFAStore{encSecret: encSecret, reads: newRendezvous(2), totpSteps: newRendezvous(2)}
	for _, c := range codes {
		s.unspent = append(s.unspent, recoveryCodeHash(c))
	}
	return s
}

func (s *racingMFAStore) GetEnabled(_ context.Context, _ uuid.UUID) (string, []string, error) {
	s.mu.Lock()
	hashes := slices.Clone(s.unspent)
	s.mu.Unlock()
	s.reads.await(0, nil)
	return s.encSecret, hashes, nil
}

func (s *racingMFAStore) UpdateBackupCodes(_ context.Context, _ uuid.UUID, hashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unspent = slices.Clone(hashes)
	return nil
}

func (s *racingMFAStore) ConsumeBackupCode(_ context.Context, _ uuid.UUID, codeHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.Index(s.unspent, codeHash)
	if i < 0 {
		return false, nil
	}
	s.unspent = slices.Delete(s.unspent, i, i+1)
	return true, nil
}

func (s *racingMFAStore) ConsumeTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (bool, error) {
	var ok bool
	s.totpSteps.await(step, func() { ok, _ = s.fakeMFAStore.ConsumeTOTPStep(ctx, userID, step) })
	return ok, nil
}

func (s *racingMFAStore) unspentHashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.unspent)
}

func recoveryCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// Two second-factor completions that arrive together sign in at most once
// for one recovery code or one challenge, and spending two recovery codes at
// once leaves both spent.
func TestMFAVerify_ConcurrentCompletionsSignInOnce(t *testing.T) {
	t.Parallel()
	const jwtSecret = "test-secret-at-least-32-bytes-long!!"
	userID := uuid.New()

	challenge := func(t *testing.T) string {
		t.Helper()
		token, err := auth.SignChallenge(jwtSecret, userID, "racer@example.com", []string{"editor"}, "")
		require.NoError(t, err)
		return token
	}
	// race sends each completion at once and returns their statuses in
	// ascending order.
	race := func(t *testing.T, store security.MFAStore, completions ...[2]string) []int {
		t.Helper()
		h := NewAuthHandler(nil, store, nil, jwtSecret, 3600, false)
		statuses := make([]int, len(completions))
		var wg sync.WaitGroup
		for i, c := range completions {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(makeMFAVerifyBody(c[0], c[1])))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.MFAVerify(rec, req)
				statuses[i] = rec.Code
			}()
		}
		wg.Wait()
		slices.Sort(statuses)
		return statuses
	}
	encrypt := func(t *testing.T, plain string) string {
		t.Helper()
		enc, err := security.EncryptSecret(plain, jwtSecret)
		require.NoError(t, err)
		return enc
	}

	t.Run("one recovery code on two challenges", func(t *testing.T) {
		t.Parallel()
		store := newRacingMFAStore(encrypt(t, "JBSWY3DPEHPK3PXP"), "ABCD-EFGH-IJKL", "MNOP-QRST-UVWX")
		got := race(t, store,
			[2]string{challenge(t), "ABCD-EFGH-IJKL"},
			[2]string{challenge(t), "ABCD-EFGH-IJKL"})
		assert.Equal(t, []int{http.StatusOK, http.StatusUnprocessableEntity}, got)
		assert.Equal(t, []string{recoveryCodeHash("MNOP-QRST-UVWX")}, store.unspentHashes())
	})

	t.Run("two recovery codes at once both stay spent", func(t *testing.T) {
		t.Parallel()
		store := newRacingMFAStore(encrypt(t, "JBSWY3DPEHPK3PXP"), "ABCD-EFGH-IJKL", "MNOP-QRST-UVWX", "YZ23-4567-89AB")
		got := race(t, store,
			[2]string{challenge(t), "ABCD-EFGH-IJKL"},
			[2]string{challenge(t), "MNOP-QRST-UVWX"})
		assert.Equal(t, []int{http.StatusOK, http.StatusOK}, got)
		assert.Equal(t, []string{recoveryCodeHash("YZ23-4567-89AB")}, store.unspentHashes())
	})

	t.Run("one challenge with two authenticator codes", func(t *testing.T) {
		t.Parallel()
		// Codes for the current and the next step are both valid and are not
		// a replay of each other, so only the challenge can refuse one.
		var plain, now, next string
		for now == next {
			var err error
			plain, _, err = security.GenerateTOTP("racer@example.com", "")
			require.NoError(t, err)
			now, err = totp.GenerateCode(plain, time.Now())
			require.NoError(t, err)
			next, err = totp.GenerateCode(plain, time.Now().Add(30*time.Second))
			require.NoError(t, err)
		}
		store := newRacingMFAStore(encrypt(t, plain))
		token := challenge(t)
		got := race(t, store, [2]string{token, now}, [2]string{token, next})
		assert.Equal(t, []int{http.StatusOK, http.StatusUnauthorized}, got)
	})
}
