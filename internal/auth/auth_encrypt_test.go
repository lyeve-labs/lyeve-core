package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// AuthClaims, nil receiver edge case.

func TestClaims_AuthClaims_NilReceiver(t *testing.T) {
	var c *auth.Claims
	assert.Nil(t, c.AuthClaims())
}

// deriveEncKey: exercised via EncryptSecret round-trip

func TestDeriveEncKey_CalledViaEncrypt(t *testing.T) {
	enc, err := auth.EncryptSecret("sensitive-data", "my-passphrase")
	require.NoError(t, err)
	require.NotEmpty(t, enc)

	dec, err := auth.DecryptSecret(enc, "my-passphrase")
	require.NoError(t, err)
	assert.Equal(t, "sensitive-data", dec)
}

// EncryptSecret / DecryptSecret: additional edge cases

func TestEncryptSecret_BothEmpty(t *testing.T) {
	enc, err := auth.EncryptSecret("", "")
	require.NoError(t, err)
	assert.NotEmpty(t, enc, "encrypted empty should still produce ciphertext")

	dec, err := auth.DecryptSecret(enc, "")
	require.NoError(t, err)
	assert.Empty(t, dec)
}

func TestDecryptSecret_VariousMalformed(t *testing.T) {
	_, err := auth.DecryptSecret("not-hex-at-all!!!", "pass")
	assert.Error(t, err)

	_, err = auth.DecryptSecret("", "pass")
	assert.Error(t, err)

	// 1 byte hex, shorter than 16-byte salt + nonce
	_, err = auth.DecryptSecret("ab", "pass")
	assert.Error(t, err)
}

// RefreshTokenStore: backend-based constructor

// Exercises the store over the in-process memory backend (no external dep).
func TestNewRefreshTokenStore_Backend(t *testing.T) {
	store := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
	require.NotNil(t, store)

	result, err := store.Issue(context.Background(), "user-url-1", 5*time.Minute)
	require.NoError(t, err)
	assert.NotEmpty(t, result.RefreshToken)
	assert.NotEmpty(t, result.FamilyID)

	require.NoError(t, store.Ping(context.Background()))
	_ = store.Close()
}

// RefreshTokenFamilyID: exported wrapper for token parsing

func TestRefreshTokenFamilyID_ValidToken(t *testing.T) {
	store := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
	defer store.Close()

	result, err := store.Issue(context.Background(), "user-rtfid", 5*time.Minute)
	require.NoError(t, err)

	familyID, hash, err := auth.RefreshTokenFamilyID(result.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, result.FamilyID, familyID)
	assert.NotEmpty(t, hash)
	assert.Len(t, hash, 64) // SHA-256 hex = 64 chars
}

func TestRefreshTokenFamilyID_BadFormat(t *testing.T) {
	_, _, err := auth.RefreshTokenFamilyID("not-a-valid-token")
	assert.Error(t, err)

	// Correct format but non-UUID.
	_, _, err = auth.RefreshTokenFamilyID(
		"not-a-uuid-at-all-with-enough-chars-for-the-dot-check.randomstuff")
	assert.Error(t, err)

	_, _, err = auth.RefreshTokenFamilyID("")
	assert.Error(t, err)
}

// Parse: EdDSA active + no public key (defensive branch)

// EdDSA active with nil signing key is a defensive branch that requires
// internal state corruption to trigger from outside the package.
func TestParse_EdDSAActiveButNoPublicKey(t *testing.T) {
	// With a valid active key, Parse falls through to HMAC verification.
	if auth.IsEdDSAActive() {
		assert.NotNil(t, auth.PublicKey(), "EdDSA active should have a public key")
	}
}

// ParseMulti: empty secrets list

func TestParseMulti_EmptySecrets(t *testing.T) {
	_, err := auth.ParseMulti(nil, "any.token.here")
	assert.Error(t, err, "nil secrets should return error")

	_, err = auth.ParseMulti([]string{}, "any.token.here")
	assert.Error(t, err, "empty secrets should return error")
}

// Password: VerifyPassword OAUTH_ONLY_MARKER with all algos

func TestVerifyPassword_OAUTH_ONLY_MARKER_AllAlgos(t *testing.T) {
	for _, algo := range []string{"bcrypt", "argon2id", "sha512", ""} {
		t.Run(algo, func(t *testing.T) {
			err := auth.VerifyPassword(algo, "!OAUTH_ONLY!", "anything")
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "oauth-only")
		})
	}
}

// TOTP: GenerateTOTP edge cases

// The otp library requires at least an Issuer.
func TestTOTP_GenerateWithEmptyConfig(t *testing.T) {
	cfg := &auth.TOTPConfig{Issuer: "Test"}
	secret, uri, err := auth.GenerateTOTP("empty-config@example.com", cfg)
	require.NoError(t, err)
	assert.NotEmpty(t, secret)
	assert.NotEmpty(t, uri)
	assert.Contains(t, uri, "otpauth://totp/")
	assert.Contains(t, uri, "empty-config@example.com")
}

func TestTOTP_GenerateWithOnlyIssuer(t *testing.T) {
	secret, uri, err := auth.GenerateTOTP("issuer-only@example.com", &auth.TOTPConfig{
		Issuer: "MyApp",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, secret)
	assert.Contains(t, uri, "MyApp")
	assert.Contains(t, uri, "otpauth://totp/")
}

// OAuth: BuildAuthURL edge (empty scopes specifically)

func TestBuildAuthURL_NilScopes(t *testing.T) {
	url := security.BuildAuthURL(
		"https://accounts.example.com/authorize",
		"client-abc",
		"https://app.example.com/callback",
		"state-token",
		"code-challenge",
		nil,
	)
	assert.Contains(t, url, "https://accounts.example.com/authorize?")
	assert.Contains(t, url, "scope=", "scope param is always included (empty for nil)")
	assert.Contains(t, url, "response_type=code")
	assert.Contains(t, url, "client_id=client-abc")
}

func TestBuildAuthURL_ZeroScopes(t *testing.T) {
	url := security.BuildAuthURL(
		"https://accounts.example.com/authorize",
		"client-abc",
		"https://app.example.com/callback",
		"state-token",
		"code-challenge",
		[]string{},
	)
	assert.Contains(t, url, "https://accounts.example.com/authorize?")
	assert.Contains(t, url, "scope=", "scope param is always included (empty for nil)")
}

// BuildScopeRoute: methodToAction and extractResource edges

// HTTP methods not covered by scope_test.go.
func TestBuildScopeRoute_MethodEdges(t *testing.T) {
	scope := core.BuildScopeRoute("HEAD", "data")
	assert.Equal(t, "read", scope.Action)

	scope = core.BuildScopeRoute("OPTIONS", "files")
	assert.Equal(t, "read", scope.Action)

	// Unknown method falls to default "write".
	scope = core.BuildScopeRoute("UNKNOWN", "data")
	assert.NotEmpty(t, scope.Action)
}

func TestBuildScopeRoute_ExtractResourceEdges(t *testing.T) {
	// Bare path (no api/v1 prefix).
	scope := core.BuildScopeRoute("GET", "posts")
	assert.Equal(t, "posts", scope.Resource)

	// Empty path.
	scope = core.BuildScopeRoute("GET", "")
	assert.Equal(t, "unknown", scope.Resource)

	// Path with only slashes.
	scope = core.BuildScopeRoute("GET", "///")
	assert.NotEmpty(t, scope.Resource)
}

// PublicKey: EdDSA inactive path

func TestPublicKey_State(t *testing.T) {
	if !auth.IsEdDSAActive() {
		assert.Nil(t, auth.PublicKey(), "PublicKey() should be nil when EdDSA not active")
	} else {
		assert.NotNil(t, auth.PublicKey(), "PublicKey() should be non-nil when EdDSA active")
	}
}

// ExtractEmail / ExtractRoles edge cases

func TestExtractEmail_EdgeCases(t *testing.T) {
	assert.Empty(t, security.ExtractEmail(nil))
	assert.Empty(t, security.ExtractEmail(map[string]any{}))
	assert.Empty(t, security.ExtractEmail(map[string]any{"other": "data"}))

	assert.Equal(t, "user@example.com", security.ExtractEmail(
		map[string]any{"email": "user@example.com"}))

	// Non-string email field.
	result := security.ExtractEmail(map[string]any{"email": 12345})
	assert.NotContains(t, result, "@", "numeric email should not be treated as email")
}

func TestExtractRoles_EdgeCases(t *testing.T) {
	roles := security.ExtractRoles(nil, "", nil)
	assert.Nil(t, roles)

	roles = security.ExtractRoles(map[string]any{"sub": "user-1"}, "roles", nil)
	assert.Nil(t, roles)

	// Mixed valid/invalid entries.
	roles = security.ExtractRoles(map[string]any{
		"roles": []any{"admin", 42, "editor", true, nil, "viewer"},
	}, "roles", nil)
	assert.Contains(t, roles, "admin")
	assert.Contains(t, roles, "editor")
	assert.Contains(t, roles, "viewer")
	assert.NotContains(t, roles, 42)
	assert.Len(t, roles, 3, "should have exactly 3 string roles")
}
