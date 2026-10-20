package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// The code step tells an account it has no authenticator app only when the
// store says so. A store that cannot answer is an outage the client retries,
// not a reason to tell the reader the account has no app.
func TestMFAVerify_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	const secret = "test-secret-at-least-32-bytes-long!!"
	verify := func(store security.MFAStore) (int, string) {
		h := NewAuthHandler(nil, store, nil, secret, 3600, false)
		challenge, err := auth.SignChallenge(secret, uuid.New(), "a@example.com", []string{"editor"}, "")
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(makeMFAVerifyBody(challenge, "123456")))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.MFAVerify(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := verify(&failingMFAStore{})
	assert.Equal(t, http.StatusServiceUnavailable, code, body)
	assert.Contains(t, body, string(i18n.CodeMFAStoreError))

	code, body = verify(&passkeyOnlyStore{})
	assert.Equal(t, http.StatusUnauthorized, code, body)
	assert.Contains(t, body, string(i18n.CodeMFANotConfigured))
}
