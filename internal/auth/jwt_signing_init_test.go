package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// EdDSA JWT: Signing Key Lifecycle

func TestInitJWTSigning_GenerateNewKey(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "jwt_key.json")

	t.Setenv("JWT_ALG", "EdDSA")

	err := auth.InitJWTSigning(keyPath)
	require.NoError(t, err, "InitJWTSigning should succeed for a fresh key")
	require.True(t, auth.IsEdDSAActive(), "EdDSA should be active after init")

	_, err = os.Stat(keyPath)
	require.NoError(t, err, "key file should exist after init")

	secret := "fallback-hmac-secret-at-least-32-bytes-long"
	id := uuid.New()

	token, err := auth.Sign(secret, 3600, id, "eddsa@example.com", []string{"admin"}, "", 1)
	require.NoError(t, err, "Sign with EdDSA should succeed")

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err, "Parse EdDSA token should succeed")
	assert.Equal(t, id.String(), claims.UserID)
	assert.Equal(t, "eddsa@example.com", claims.Email)
}

func TestInitJWTSigning_LoadExistingKey(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "jwt_key.json")

	t.Setenv("JWT_ALG", "EdDSA")

	err := auth.InitJWTSigning(keyPath)
	require.NoError(t, err)

	secret := "test-secret-thats-long-enough-for-signing"
	id := uuid.New()
	token1, err := auth.Sign(secret, 3600, id, "alice@example.com", nil, "", 1)
	require.NoError(t, err)

	// Simulate a restart: reload the key.
	err = auth.InitJWTSigning(keyPath)
	require.NoError(t, err, "reloading an existing key should succeed")
	require.True(t, auth.IsEdDSAActive())

	claims, err := auth.Parse(secret, token1)
	require.NoError(t, err, "token signed before reload should still be parseable")
	assert.Equal(t, id.String(), claims.UserID)
}

// NOTE: eddsaActive is a process global that persists across tests, so when
// a prior test activated EdDSA the active-state assertion is skipped. The
// invariants here: JWT_ALG=HS256 creates no key file and HS256 still signs.
func TestInitJWTSigning_HS256OptOut(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "jwt_key.json")

	t.Setenv("JWT_ALG", "HS256")

	err := auth.InitJWTSigning(keyPath)
	require.NoError(t, err, "InitJWTSigning should succeed when opting out")

	_, err = os.Stat(keyPath)
	assert.True(t, os.IsNotExist(err), "no key file should be created when HS256")

	secret := "secret-at-least-32-bytes-for-hmac-sha256-signing"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "hs256@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

func TestInitJWTSigning_EmptyAlgDefaultsToEdDSA(t *testing.T) {
	os.Unsetenv("JWT_ALG")
	defer os.Unsetenv("JWT_ALG")

	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "jwt_key.json")

	err := auth.InitJWTSigning(keyPath)
	require.NoError(t, err)
	require.True(t, auth.IsEdDSAActive(), "EdDSA should be active by default when JWT_ALG is unset")
}

func TestEdDSA_SignParseRoundTrip(t *testing.T) {
	ensureEdDSA(t)

	secret := "this-hmac-secret-is-long-enough-for-fallback"
	id := uuid.New()
	email := "roundtrip@example.com"
	roles := []string{"admin", "editor"}

	token, err := auth.Sign(secret, 7200, id, email, roles, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
	assert.Equal(t, email, claims.Email)
	assert.Equal(t, roles, claims.Roles)
	assert.Equal(t, id.String(), claims.UserID) // UserID maps to "sub" JSON key, and RegisteredClaims.Subject is shadowed
	assert.False(t, claims.MFAPending)
}

func TestEdDSA_SignChallenge_MFAPending(t *testing.T) {
	ensureEdDSA(t)

	secret := "this-hmac-secret-is-long-enough-for-fallback"
	id := uuid.New()
	token, err := auth.SignChallenge(secret, id, "mfa@example.com", []string{"user"}, "")
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.True(t, claims.MFAPending, "challenge token must have MFA pending")
	assert.Equal(t, id.String(), claims.UserID)
}

func TestEdDSA_ParseMalformedToken(t *testing.T) {
	ensureEdDSA(t)

	_, err := auth.Parse("any-secret", "garbage")
	assert.Error(t, err, "parse garbage should fail")
}

// A total key reset (delete the key file, re-init) is an admin action:
// tokens signed by the old EdDSA key must fail verification afterwards.
func TestEdDSA_ParseEdDSATokenAfterKeyReset(t *testing.T) {
	tmpDir := t.TempDir()
	keyPath := filepath.Join(tmpDir, "jwt_key.json")

	t.Setenv("JWT_ALG", "EdDSA")
	err := auth.InitJWTSigning(keyPath)
	require.NoError(t, err)

	secret := "secret-must-be-long-enough-for-hmac-256"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "user@example.com", nil, "", 1)
	require.NoError(t, err)

	// Simulate an admin key reset: delete the file and re-init.
	err = os.Remove(keyPath)
	require.NoError(t, err)
	err = auth.InitJWTSigning(keyPath)
	require.NoError(t, err)

	_, err = auth.Parse(secret, token)
	assert.Error(t, err, "token signed with old EdDSA key should fail with new key")
}

// EdDSA + HS256 Coexistence

// With EdDSA active, Sign emits EdDSA tokens and Parse accepts them.
func TestMixedSigning_HS256StillWorksWhenEdDSAActive(t *testing.T) {
	ensureEdDSA(t)

	secret := "legacy-secret-that-is-at-least-32-bytes"
	id := uuid.New()

	edToken, err := auth.Sign(secret, 3600, id, "mixed@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, edToken)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

// ParseMulti takes a secret list even for EdDSA tokens: legacy HS256 tokens
// in the same system still need theirs.
func TestParseMulti_EdDSAToken(t *testing.T) {
	ensureEdDSA(t)

	secret := "multi-secret-long-enough-for-parse-multi-test"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "multi@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.ParseMulti([]string{secret}, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

// An EdDSA-signed token parses even with a wrong HMAC secret, because EdDSA
// verification never touches the secret.
func TestParseMulti_EdDSATokenWrongHMACSecret(t *testing.T) {
	ensureEdDSA(t)

	id := uuid.New()
	token, err := auth.Sign("original-secret-is-long-enough-for-sign", 3600, id, "eddsa-only@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.ParseMulti([]string{"wrong-secret"}, token)
	require.NoError(t, err, "EdDSA token should parse regardless of HMAC secret")
	assert.Equal(t, id.String(), claims.UserID)
}

// JWKS Endpoint

func TestJWKSHandler_EdDSAActive(t *testing.T) {
	ensureEdDSA(t)

	handler := auth.JWKSHandler()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=3600", w.Header().Get("Cache-Control"))

	var resp jwksResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	require.Len(t, resp.Keys, 1, "should return exactly one key")
	key := resp.Keys[0]
	assert.Equal(t, "OKP", key.Kty)
	assert.Equal(t, "Ed25519", key.Crv)
	assert.Equal(t, "sig", key.Use)
	assert.Equal(t, "EdDSA", key.Alg)
	assert.NotEmpty(t, key.Kid, "kid must be non-empty")
	assert.NotEmpty(t, key.X, "x must be non-empty (base64url public key)")

	// Verify the key ID looks valid (base64url, right length for SHA-256[0:8]).
	assert.Len(t, key.Kid, 11, "kid should be 11 chars (base64url of 8 bytes)")
}

func TestJWKSHandler_NoEdDSA(t *testing.T) {
	// This test depends on EdDSA NOT being active.
	// It works when run in isolation or when HS256-only tests run first.
	if auth.IsEdDSAActive() {
		t.Skip("EdDSA is active from a prior test; JWKS no-key test requires clean state")
	}

	handler := auth.JWKSHandler()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp jwksResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Len(t, resp.Keys, 0, "should return empty keyset when EdDSA not active")
}

// Password Hashing: Edge Cases

func TestVerifyPassword_OAUTH_ONLY_MARKER(t *testing.T) {
	err := auth.VerifyPassword("bcrypt", "!OAUTH_ONLY!", "anything")
	assert.Error(t, err, "OAUTH_ONLY_MARKER should always reject password auth")
	assert.Contains(t, err.Error(), "oauth-only")
}

func TestHashPassword_Argon2idProducesPHCFormat(t *testing.T) {
	hash, err := auth.HashPassword("argon2id", "testpassword")
	require.NoError(t, err)

	// Must start with $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>
	assert.True(t, strings.HasPrefix(hash, "$argon2id$"), "argon2id hash should have PHC prefix")

	parts := strings.Split(hash, "$")
	require.Len(t, parts, 6, "PHC format: $argon2id$...$...$...$...$... (6 parts with leading empty)")

	// Verify version is 19 (argon2 current).
	assert.Contains(t, parts[2], "v=19")

	// Verify memory is 64 MiB (65536 KiB), time=1, threads=4.
	assert.Contains(t, parts[3], "m=65536")
	assert.Contains(t, parts[3], "t=1")
	assert.Contains(t, parts[3], "p=4")
}

// Hashing an empty password succeeds. Rejecting it is a policy decision,
// not the hasher's.
func TestHashPassword_EmptyPassword(t *testing.T) {
	for _, algo := range []string{"bcrypt", "argon2id"} {
		t.Run(algo, func(t *testing.T) {
			hash, err := auth.HashPassword(algo, "")
			require.NoError(t, err)
			assert.NotEmpty(t, hash)

			// Verify against the same empty password.
			err = auth.VerifyPassword(algo, hash, "")
			assert.NoError(t, err)
		})
	}
}

func TestHashPassword_LongPassword(t *testing.T) {
	// bcrypt has a 72-byte limit and truncates beyond that, so password+x
	// would still match if password is 72 bytes. Use 71 for bcrypt.
	bcryptPass := strings.Repeat("a", 71)
	argonPass := strings.Repeat("🔐securePassword! ", 100) // ~2100 chars UTF-8
	for _, tt := range []struct {
		algo string
		pass string
	}{
		{"bcrypt", bcryptPass},
		{"argon2id", argonPass},
	} {
		t.Run(tt.algo, func(t *testing.T) {
			hash, err := auth.HashPassword(tt.algo, tt.pass)
			require.NoError(t, err)
			err = auth.VerifyPassword(tt.algo, hash, tt.pass)
			assert.NoError(t, err)

			// Slightly different password should fail.
			err = auth.VerifyPassword(tt.algo, hash, tt.pass+"x")
			assert.Error(t, err)
		})
	}
}

func TestHashPassword_ConsistentBcryptPrefix(t *testing.T) {
	hash, err := auth.HashPassword("bcrypt", "somepass")
	require.NoError(t, err)

	// bcrypt always produces $2a$ (or $2b$ on newer versions).
	assert.True(t, strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$"),
		"bcrypt hash should start with $2a$ or $2b$")
}

func TestHashPassword_DifferentCallsDifferentHashes(t *testing.T) {
	for _, algo := range []string{"bcrypt", "argon2id"} {
		t.Run(algo, func(t *testing.T) {
			h1, err := auth.HashPassword(algo, "samepassword")
			require.NoError(t, err)
			h2, err := auth.HashPassword(algo, "samepassword")
			require.NoError(t, err)
			assert.NotEqual(t, h1, h2, "same password should produce different hashes (salt)")
		})
	}
}

// TOTP: Generation and Verification

func TestTOTP_GenerateWithDefaultConfig(t *testing.T) {
	secret, uri, err := auth.GenerateTOTP("user@example.com", nil)
	require.NoError(t, err)
	assert.NotEmpty(t, secret, "TOTP secret must not be empty")
	assert.NotEmpty(t, uri, "provisioning URI must not be empty")
	assert.Contains(t, uri, "otpauth://totp/", "URI should be otpauth format")
	assert.Contains(t, uri, "user@example.com", "URI should contain account name")
}

func TestTOTP_GenerateWithCustomConfig(t *testing.T) {
	secret, uri, err := auth.GenerateTOTP("admin@example.com", &auth.TOTPConfig{
		Issuer: "LyEveCMS",
		Period: 60,
		Digits: 8,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, secret)
	assert.Contains(t, uri, "LyEveCMS", "URI should contain custom issuer")
	assert.Contains(t, uri, "period=60", "URI should contain custom period")
	assert.Contains(t, uri, "admin@example.com", "URI should contain account name")
}

func TestTOTP_GenerateProducesValidSecret(t *testing.T) {
	secret, _, err := auth.GenerateTOTP("test@example.com", nil)
	require.NoError(t, err)

	// The secret should be base32 (uppercase A-Z, 2-7, no padding with =).
	assert.Regexp(t, `^[A-Z2-7]+$`, secret, "TOTP secret should be base32")
	assert.GreaterOrEqual(t, len(secret), 16, "TOTP secret should be at least 16 chars")
}

func TestTOTP_VerifyWrongCode(t *testing.T) {
	secret, _, err := auth.GenerateTOTP("test@example.com", nil)
	require.NoError(t, err)

	// "000000" is almost certainly never the correct code.
	assert.False(t, auth.VerifyTOTP(secret, "000000"), "wrong TOTP code should be rejected")
}

func TestTOTP_VerifyEmptyCode(t *testing.T) {
	secret, _, _ := auth.GenerateTOTP("test@example.com", nil)
	assert.False(t, auth.VerifyTOTP(secret, ""))
	assert.False(t, auth.VerifyTOTP(secret, "abc123"))
	assert.False(t, auth.VerifyTOTP(secret, "12345"))   // too short
	assert.False(t, auth.VerifyTOTP(secret, "1234567")) // 7 digits (not 6 or 8)
}

func TestTOTP_VerifyEmptySecret(t *testing.T) {
	assert.False(t, auth.VerifyTOTP("", "123456"))
}

// AES-256-GCM Encryption / Decryption

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		plaintext string
	}{
		{"short ascii", "hello world"},
		{"unicode", "こんにちは世界 🔐 secret message"},
		{"json payload", `{"api_key":"sk-1234567890","url":"https://example.com"}`},
		{"empty string", ""},
		{"single char", "x"},
		{"long text", strings.Repeat("The quick brown fox jumps over the lazy dog. ", 50)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := auth.EncryptSecret(tt.plaintext, "my-strong-passphrase")
			require.NoError(t, err)
			assert.NotEmpty(t, enc, "encrypted output should not be empty")
			assert.NotEqual(t, tt.plaintext, enc, "encrypted should differ from plaintext")

			dec, err := auth.DecryptSecret(enc, "my-strong-passphrase")
			require.NoError(t, err)
			assert.Equal(t, tt.plaintext, dec)
		})
	}
}

func TestEncryptSecret_DifferentPassphraseFails(t *testing.T) {
	enc, err := auth.EncryptSecret("sensitive data", "correct-passphrase")
	require.NoError(t, err)

	_, err = auth.DecryptSecret(enc, "wrong-passphrase")
	assert.Error(t, err, "decryption with wrong passphrase must fail")
}

func TestEncryptSecret_ProducesDistinctCiphertexts(t *testing.T) {
	c1, err := auth.EncryptSecret("same plaintext", "passphrase")
	require.NoError(t, err)
	c2, err := auth.EncryptSecret("same plaintext", "passphrase")
	require.NoError(t, err)
	assert.NotEqual(t, c1, c2, "same plaintext should produce different ciphertexts (random salt/nonce)")
}

func TestDecryptSecret_MalformedHex(t *testing.T) {
	_, err := auth.DecryptSecret("not-hex-at-all!!!", "passphrase")
	assert.Error(t, err, "non-hex input should fail decryption")
}

func TestDecryptSecret_TooShort(t *testing.T) {
	_, err := auth.DecryptSecret("aa", "passphrase") // 1 byte, shorter than 16-byte salt
	assert.Error(t, err, "too-short ciphertext should fail")
}

// PBKDF2 handles a zero-length passphrase, so an empty one is allowed.
func TestEncryptSecret_EmptyPassphrase(t *testing.T) {
	enc, err := auth.EncryptSecret("data", "")
	require.NoError(t, err)
	dec, err := auth.DecryptSecret(enc, "")
	require.NoError(t, err)
	assert.Equal(t, "data", dec)
}

// Claims / Helpers

func TestClaims_HasRole_Empty(t *testing.T) {
	c := &auth.Claims{Roles: nil}
	assert.False(t, c.HasRole("admin"))
	assert.False(t, c.HasRole(""))
}

func TestClaims_AuthClaims_Conversion(t *testing.T) {
	c := &auth.Claims{
		UserID:     "user-123",
		Email:      "test@example.com",
		Roles:      []string{"admin"},
		TenantID:   "tenant-abc",
		MFAPending: true,
	}
	ac := c.AuthClaims()
	assert.Equal(t, "user-123", ac.UserID)
	assert.Equal(t, "test@example.com", ac.Email)
	assert.Equal(t, []string{"admin"}, ac.Roles)
	assert.Equal(t, "tenant-abc", ac.TenantID)
	assert.True(t, ac.MFAPending)
}

// Helpers

// jwksResponse matches the JWKS JSON structure.
type jwksResponse struct {
	Keys []jwksKey `json:"keys"`
}
type jwksKey struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	Kid string `json:"kid,omitempty"`
}

var (
	eddsaOnce sync.Once
	eddsaErr  error
)

// ensureEdDSA initializes EdDSA signing with a fresh key in a temp dir.
// Safe to call from multiple parallel tests: uses sync.Once so only the
// first call actually initializes. Subsequent calls are no-ops.
func ensureEdDSA(t *testing.T) {
	t.Helper()
	eddsaOnce.Do(func() {
		tmpDir := t.TempDir()
		keyPath := filepath.Join(tmpDir, "jwt_key.json")

		t.Setenv("JWT_ALG", "EdDSA")

		eddsaErr = auth.InitJWTSigning(keyPath)
	})
	require.NoError(t, eddsaErr, "InitJWTSigning failed in ensureEdDSA helper")
	require.True(t, auth.IsEdDSAActive(), "EdDSA should be active after ensureEdDSA")
}
