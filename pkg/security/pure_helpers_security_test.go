package security_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

const jwtTestSecret = "test-secret-at-least-32-bytes-long!"

// oauth.go (pure helpers only, network calls are integration-only)

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := security.GeneratePKCE()
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(verifier)
	require.NoError(t, err)
	assert.Len(t, raw, 32)

	sum := sha256.Sum256([]byte(verifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), challenge)

	v2, _, err := security.GeneratePKCE()
	require.NoError(t, err)
	assert.NotEqual(t, verifier, v2, "verifier must be random per call")
}

func TestGenerateState(t *testing.T) {
	s, err := security.GenerateState()
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(s)
	require.NoError(t, err)
	assert.Len(t, raw, 16)

	s2, err := security.GenerateState()
	require.NoError(t, err)
	assert.NotEqual(t, s, s2)
}

func TestBuildAuthURL(t *testing.T) {
	raw := security.BuildAuthURL("https://idp.example/authorize", "client123",
		"https://app.example/cb", "state-xyz", "chal-abc", []string{"openid", "email"})

	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "https://idp.example/authorize", u.Scheme+"://"+u.Host+u.Path)

	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "client123", q.Get("client_id"))
	assert.Equal(t, "https://app.example/cb", q.Get("redirect_uri"))
	assert.Equal(t, "state-xyz", q.Get("state"))
	assert.Equal(t, "chal-abc", q.Get("code_challenge"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.Equal(t, "openid email", q.Get("scope"))
}

func TestExtractEmail(t *testing.T) {
	assert.Equal(t, "a@b.com", security.ExtractEmail(map[string]any{"email": "a@b.com"}))
	assert.Equal(t, "", security.ExtractEmail(map[string]any{}))
	assert.Equal(t, "", security.ExtractEmail(map[string]any{"email": 123}))
}

func TestExtractRoles(t *testing.T) {
	def := []string{"viewer"}
	assert.Equal(t, def, security.ExtractRoles(map[string]any{}, "", def), "empty claim name -> default")
	assert.Equal(t, def, security.ExtractRoles(map[string]any{}, "roles", def), "absent claim -> default")
	assert.Equal(t, []string{"editor", "author"},
		security.ExtractRoles(map[string]any{"roles": []any{"editor", "author"}}, "roles", def))
	assert.Equal(t, def,
		security.ExtractRoles(map[string]any{"roles": []any{123, true}}, "roles", def), "no string entries -> default")
	assert.Equal(t, []string{"admin"},
		security.ExtractRoles(map[string]any{"roles": "admin"}, "roles", def))
	assert.Equal(t, def,
		security.ExtractRoles(map[string]any{"roles": ""}, "roles", def), "empty string -> default")
	assert.Equal(t, def,
		security.ExtractRoles(map[string]any{"roles": 42}, "roles", def), "unsupported type -> default")
}

func TestFilterIDPRoles(t *testing.T) {
	assert.Equal(t, []string{"editor", "viewer"},
		security.FilterIDPRoles([]string{"editor", "admin", "viewer", "super_admin"}))
	assert.Equal(t, []string{}, security.FilterIDPRoles([]string{"admin", "super_admin"}))
	assert.Empty(t, security.FilterIDPRoles(nil))
}

// jwt_helpers.go (pure / HS256 helpers)

func TestGenerateNonce(t *testing.T) {
	n, err := security.GenerateNonce()
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(n)
	require.NoError(t, err)
	assert.Len(t, raw, 32)

	n2, err := security.GenerateNonce()
	require.NoError(t, err)
	assert.NotEqual(t, n, n2)
}

func TestSignMap_ParseMap_HS256Roundtrip(t *testing.T) {
	// Force the HMAC path regardless of any bridge left by another test.
	orig := security.EdDSASigningKey
	security.EdDSASigningKey = nil
	t.Cleanup(func() { security.EdDSASigningKey = orig })

	claims := jwt.MapClaims{
		"sub": "user-1",
		"foo": "bar",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	tokenStr, err := security.SignMap(jwtTestSecret, claims)
	require.NoError(t, err)
	require.NotEmpty(t, tokenStr)

	parsed, err := security.ParseMap(jwtTestSecret, tokenStr)
	require.NoError(t, err)
	assert.Equal(t, "user-1", parsed["sub"])
	assert.Equal(t, "bar", parsed["foo"])
}

func TestParseMap_Rejects(t *testing.T) {
	orig := security.EdDSASigningKey
	security.EdDSASigningKey = nil
	t.Cleanup(func() { security.EdDSASigningKey = orig })

	// Garbage token.
	_, err := security.ParseMap(jwtTestSecret, "not.a.jwt")
	assert.Error(t, err)

	// Signed with a different secret -> signature mismatch.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "x",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString([]byte("some-other-secret-value-32bytes!!"))
	require.NoError(t, err)
	_, err = security.ParseMap(jwtTestSecret, signed)
	assert.Error(t, err)
}

func TestExtractEmailVerified(t *testing.T) {
	assert.True(t, security.ExtractEmailVerified(map[string]any{"email_verified": true}))
	assert.False(t, security.ExtractEmailVerified(map[string]any{"email_verified": false}))
	assert.False(t, security.ExtractEmailVerified(map[string]any{}))
	assert.False(t, security.ExtractEmailVerified(map[string]any{"email_verified": "true"}), "non-bool must be false")
}

// totp.go

func TestGenerateTOTP(t *testing.T) {
	secret, uri, err := security.GenerateTOTP("user@example.com", "")
	require.NoError(t, err)
	assert.NotEmpty(t, secret)
	assert.True(t, strings.HasPrefix(uri, "otpauth://totp/"), "uri=%s", uri)

	// Parsed rather than substring-matched. The issuer is the name a user reads
	// in their authenticator for the life of the account, and a Contains check
	// passes for any string that merely embeds the expected one.
	parsed, err := url.Parse(uri)
	require.NoError(t, err)
	assert.Equal(t, "LyEve", parsed.Query().Get("issuer"),
		"an empty issuer takes the product default")
	assert.Equal(t, "/LyEve:user@example.com", parsed.Path,
		"the label carries the issuer too, which is what older apps display")
	// Written as a literal, not as the constant. Comparing the constant to
	// itself would pass whatever the constant became, and the point of the
	// assertion is that it names this product.
	assert.Equal(t, "LyEve", security.DefaultTOTPIssuer)

	_, uri2, err := security.GenerateTOTP("user@example.com", "MyApp")
	require.NoError(t, err)
	parsed2, err := url.Parse(uri2)
	require.NoError(t, err)
	assert.Equal(t, "MyApp", parsed2.Query().Get("issuer"), "a named issuer is used as given")
}

func TestVerifyTOTPNoReplay(t *testing.T) {
	secret, _, err := security.GenerateTOTP("user@example.com", "MyApp")
	require.NoError(t, err)

	now := time.Now().UTC()
	code, err := totp.GenerateCode(secret, now)
	require.NoError(t, err)

	ok, ts := security.VerifyTOTPNoReplay(secret, code, 0)
	require.True(t, ok, "freshly generated code must validate")
	assert.Equal(t, now.Unix()/30, ts)

	// Replay within the same (or earlier) timestep is rejected.
	replayOK, replayTS := security.VerifyTOTPNoReplay(secret, code, ts)
	assert.False(t, replayOK)
	assert.Zero(t, replayTS)

	// A code that cannot be the correct 6-digit value is rejected.
	badOK, badTS := security.VerifyTOTPNoReplay(secret, "abc", 0)
	assert.False(t, badOK)
	assert.Zero(t, badTS)
}

// backup codes

func TestGenerateBackupCodes(t *testing.T) {
	codes, err := security.GenerateBackupCodes(5)
	require.NoError(t, err)
	require.Len(t, codes, 5)
	seen := map[string]bool{}
	for _, c := range codes {
		// The number that matters is the entropy, not the length. A code
		// carries all 64 bits, because it is stored as an unsalted SHA-256
		// and a shorter one is an offline search rather than a guessing
		// attack.
		raw, err := hex.DecodeString(c)
		assert.NoError(t, err, "code must be valid hex: %s", c)
		assert.Len(t, raw, 8, "a backup code carries 64 bits, and none of them is thrown away")
		seen[c] = true
	}
	assert.Len(t, seen, 5, "codes should be unique")

	empty, err := security.GenerateBackupCodes(0)
	require.NoError(t, err)
	assert.Empty(t, empty)
}
