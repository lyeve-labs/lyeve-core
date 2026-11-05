package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

const signInTimeSecret = "sign-in-time-secret-at-least-32-bytes!"

// eachAccountDialect runs fn against a fresh account store on every dialect
// the run enables.
func eachAccountDialect(t *testing.T, fn func(t *testing.T, store *db.UserStore)) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		fn(t, db.NewUserStore(pool))
	})
}

// parseSession reads the claims of a session token the handler answered with.
func parseSession(t *testing.T, token string) *auth.Claims {
	t.Helper()
	require.NotEmpty(t, token)
	claims, err := auth.Parse(signInTimeSecret, token)
	require.NoError(t, err)
	return claims
}

// A refresh renews the session and never the sign-in. The access token a
// refresh signs carries the time the person signed in, however many times the
// family has rotated, while its own issue time moves.
func TestRefresh_KeepsTheSignInTime(t *testing.T) {
	eachAccountDialect(t, func(t *testing.T, store *db.UserStore) {
		ctx := context.Background()
		hash, err := auth.HashPassword("bcrypt", "the-sign-in-password-1")
		require.NoError(t, err)
		email := "sign-in-time-" + uuid.NewString()[:8] + "@example.com"
		user, err := store.Create(ctx, email, hash, []string{"editor"}, "")
		require.NoError(t, err)

		refresh := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
		h := NewAuthHandler(store, nil, nil, signInTimeSecret, 3600, false)
		h.WithRefreshTokenStore(refresh, 15*time.Minute)

		trade := func(t *testing.T, token string) (*auth.Claims, string) {
			t.Helper()
			body, err := json.Marshal(map[string]string{"refresh_token": token})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.Refresh(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			resp := decodeAuthResponse(t, rr)
			access, _ := resp["token"].(string)
			next, _ := resp["refresh_token"].(string)
			require.NotEmpty(t, next)
			return parseSession(t, access), next
		}

		t.Run("a password sign-in", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(makeLoginBody(email, "the-sign-in-password-1")))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.Login(rr, req)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			resp := decodeAuthResponse(t, rr)
			access, _ := resp["token"].(string)
			signedIn := parseSession(t, access)
			require.NotZero(t, signedIn.AuthTime, "a sign-in stamps the time it happened")
			assert.Equal(t, signedIn.IssuedAt.Unix(), signedIn.AuthTime)

			token, _ := resp["refresh_token"].(string)
			for range 2 {
				var claims *auth.Claims
				claims, token = trade(t, token)
				assert.Equal(t, signedIn.AuthTime, claims.AuthTime, "a refresh keeps the sign-in time")
			}
		})

		t.Run("a sign-in an hour ago", func(t *testing.T) {
			signedIn := time.Now().Add(-time.Hour).Truncate(time.Second)
			issued, err := refresh.IssueForSession(ctx, user.ID.String(), "", user.TokenVersion, 15*time.Minute, signedIn)
			require.NoError(t, err)
			token := issued.RefreshToken
			for range 2 {
				var claims *auth.Claims
				claims, token = trade(t, token)
				assert.Equal(t, signedIn.Unix(), claims.AuthTime, "a refresh copies the sign-in time and never renews it")
				assert.WithinDuration(t, time.Now(), claims.IssuedAt.Time, time.Minute, "the access token itself is new")
			}
		})
	})
}

// A sign-in that needs a second factor begins when the factor passes. The
// session MFAVerify signs carries that time, and so does the refresh family
// it starts.
func TestMFAVerify_StampsTheSignInTime(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	ks, err := encryption.NewKeyStore("mfa-verify-master-key-at-least-32b!!")
	require.NoError(t, err)
	totpSecret, _, err := auth.GenerateTOTP("stamped@test.com", nil)
	require.NoError(t, err)
	encSecret, err := ks.EncryptPlaintext(mfaDefaultTenantKeyID, []byte(totpSecret))
	require.NoError(t, err)

	refresh := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
	h := NewAuthHandler(nil, &dekMFAStore{encSecret: encSecret}, nil, signInTimeSecret, 3600, false)
	h.WithKeyStore(ks)
	h.WithRefreshTokenStore(refresh, 15*time.Minute)

	challenge, err := auth.SignChallenge(signInTimeSecret, userID, "stamped@test.com", []string{"editor"}, "")
	require.NoError(t, err)
	code, err := totp.GenerateCode(totpSecret, time.Now())
	require.NoError(t, err)

	before := time.Now().Unix()
	body, err := json.Marshal(map[string]string{"challenge_token": challenge, "code": code})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.MFAVerify(rr, req)
	after := time.Now().Unix()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	resp := decodeAuthResponse(t, rr)
	access, _ := resp["token"].(string)
	claims := parseSession(t, access)
	assert.GreaterOrEqual(t, claims.AuthTime, before)
	assert.LessOrEqual(t, claims.AuthTime, after)

	token, _ := resp["refresh_token"].(string)
	require.NotEmpty(t, token)
	rotated, err := refresh.Rotate(context.Background(), token, 15*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, claims.AuthTime, rotated.AuthTime.Unix(), "the family keeps the time the factor passed")
}

// A plugin that verified a factor completes the challenge, and the session
// it gets carries the time it was signed, as the code path's does.
func TestCompleteMFAChallenge_StampsTheSignInTime(t *testing.T) {
	t.Parallel()
	c := newChallengeCompletion(t)
	challenge := c.challenge(t, "acme", []string{"editor"})

	before := time.Now().Unix()
	token, err := c.h.CompleteMFAChallenge(context.Background(), challenge)
	require.NoError(t, err)
	after := time.Now().Unix()

	claims, err := auth.Parse(challengeCompletionSecret, token)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, claims.AuthTime, before)
	assert.LessOrEqual(t, claims.AuthTime, after)
}
