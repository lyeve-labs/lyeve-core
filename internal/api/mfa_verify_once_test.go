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
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// rendezvous holds n callers until all have arrived, then lets them through
// one at a time in ascending key order. A request that returns without
// arriving calls leave, so the callers still waiting go on without it and the
// test fails on their answers instead of hanging. A deadline would not do:
// under load the requests take longer than any deadline to arrive, and
// callers let through out of order turn the race into a different one.
type rendezvous struct {
	n       int
	mu      sync.Mutex
	cond    *sync.Cond
	arrived int
	left    int
	pending []int64
}

func newRendezvous(n int) *rendezvous {
	r := &rendezvous{n: n}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *rendezvous) await(key int64, fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.arrived++
	r.pending = append(r.pending, key)
	r.cond.Broadcast()
	for r.arrived+r.left < r.n || slices.Min(r.pending) < key {
		r.cond.Wait()
	}
	if fn != nil {
		fn()
	}
	r.pending = slices.Delete(r.pending, slices.Index(r.pending, key), slices.Index(r.pending, key)+1)
	r.cond.Broadcast()
}

// leave counts a request that has returned. One that arrived was let through
// only after every request had arrived or left, so counting it as well never
// releases a caller early.
func (r *rendezvous) leave() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.left++
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

func (s *racingMFAStore) requestDone() {
	s.reads.leave()
	s.totpSteps.leave()
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
	race := func(t *testing.T, store *racingMFAStore, completions ...[2]string) []int {
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
				store.requestDone()
				statuses[i] = rec.Code
			}()
		}
		wg.Wait()
		slices.Sort(statuses)
		return statuses
	}
	// The secret is sealed under the key the handler tries first. The
	// passphrase sealing it falls back to costs a PBKDF2 stretch per request,
	// which takes seconds under -race, so this keeps each run fast.
	encrypt := func(t *testing.T, plain string) string {
		t.Helper()
		ks, err := encryption.NewKeyStoreLegacy(jwtSecret)
		require.NoError(t, err)
		dek, err := ks.DeriveTenantDEK(mfaDefaultTenantKeyID)
		require.NoError(t, err)
		enc, err := encryption.EncryptValue(dek, []byte(plain))
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
