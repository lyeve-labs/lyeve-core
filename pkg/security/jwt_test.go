package security

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jwtTestSecrets provides secrets for HS256 tests.
// Real secrets should be at least 32 bytes. 16 is the min for HS256.
const jwtTestSecret = "test-secret-at-least-32-bytes-long!"

// ed25519TestKey generates a fresh Ed25519 keypair for EdDSA test signing.
func ed25519TestKey() (ed25519.PrivateKey, ed25519.PublicKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return priv, pub
}

// TestSignJWT_Roundtrip_HS256 verifies that a token signed with SignJWT
// round-trips through ParseJWT preserving all claims with HS256.
func TestSignJWT_Roundtrip_HS256(t *testing.T) {
	userID := uuid.New()
	email := "user@example.com"
	roles := []string{"admin", "editor"}

	token, err := SignJWT(jwtTestSecret, 3600, userID, email, roles, "acme", 7)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	claims, err := ParseJWT(jwtTestSecret, token)
	require.NoError(t, err)

	assert.Equal(t, userID.String(), claims.UserID)
	assert.Equal(t, email, claims.Email)
	assert.Equal(t, roles, claims.Roles)
	assert.Equal(t, "session", claims.TokenType, "token type should be session")
	assert.False(t, claims.MFAPending, "session token should not be mfa_pending")
}

// TestSignJWT_Roundtrip_EdDSA verifies EdDSA signing when EdDSASigningKey is wired.
func TestSignJWT_Roundtrip_EdDSA(t *testing.T) {
	privateKey, publicKey := ed25519TestKey()

	// Wire the EdDSA bridge.
	kid := "test-kid-001"
	origEdDSA := EdDSASigningKey
	EdDSASigningKey = func() (ed25519.PrivateKey, string) {
		return privateKey, kid
	}
	t.Cleanup(func() { EdDSASigningKey = origEdDSA })

	// Wire the EdDSA public key for parse (set directly: SetEd25519PublicKey
	// is sync.Once-guarded and cannot be called per-test).
	origPub := eddsaPubKey
	eddsaPubKeyMu.Lock()
	eddsaPubKey = publicKey
	eddsaPubKeyMu.Unlock()
	t.Cleanup(func() {
		eddsaPubKeyMu.Lock()
		eddsaPubKey = origPub
		eddsaPubKeyMu.Unlock()
	})

	userID := uuid.New()
	email := "eddsa-user@example.com"
	roles := []string{"viewer"}

	token, err := SignJWT(jwtTestSecret, 3600, userID, email, roles, "acme", 7)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Parse with EdDSA public key.
	claims, err := ParseJWT(jwtTestSecret, token)
	require.NoError(t, err)

	assert.Equal(t, userID.String(), claims.UserID)
	assert.Equal(t, email, claims.Email)
	assert.Equal(t, roles, claims.Roles)
	assert.Equal(t, "session", claims.TokenType)
}

// TestSignJWT_EdDSA_FallbackToHS256 verifies fallback when bridge returns nil.
func TestSignJWT_EdDSA_FallbackToHS256(t *testing.T) {
	// Bridge that says "not active".
	origEdDSA := EdDSASigningKey
	EdDSASigningKey = func() (ed25519.PrivateKey, string) {
		return nil, ""
	}
	t.Cleanup(func() { EdDSASigningKey = origEdDSA })

	userID := uuid.New()
	token, err := SignJWT(jwtTestSecret, 3600, userID, "test@x.com", nil, "acme", 7)
	require.NoError(t, err)

	claims, err := ParseJWT(jwtTestSecret, token)
	require.NoError(t, err)
	assert.Equal(t, "session", claims.TokenType)
}

// TestParseJWT_RejectsTokenWithWrongIssuer verifies issuer validation.
func TestParseJWT_RejectsTokenWithWrongIssuer(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": uuid.New().String(),
		"iss": "evil-issuer",
		"aud": JWTAudience,
		"iat": 1000,
		"exp": 9999999999,
	})
	token, err := tok.SignedString([]byte(jwtTestSecret))
	require.NoError(t, err)

	_, err = ParseJWT(jwtTestSecret, token)
	assert.Error(t, err, "should reject token with wrong issuer")
}

// TestParseJWT_RejectsTokenWithWrongAudience verifies audience validation.
func TestParseJWT_RejectsTokenWithWrongAudience(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": uuid.New().String(),
		"iss": JWTIssuer,
		"aud": "wrong-audience",
		"iat": 1000,
		"exp": 9999999999,
	})
	token, err := tok.SignedString([]byte(jwtTestSecret))
	require.NoError(t, err)

	_, err = ParseJWT(jwtTestSecret, token)
	assert.Error(t, err, "should reject token with wrong audience")
}

// TestParseJWT_TokenTypeIsSet verifies TokenType flows through.
func TestParseJWT_TokenTypeIsSet(t *testing.T) {
	userID := uuid.New()

	tests := []struct {
		name       string
		typ        string
		wantType   string
		mfaPending bool
	}{
		{"session", "session", "session", false},
		{"challenge", "challenge", "challenge", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			claims := jwt.MapClaims{
				"sub":         userID.String(),
				"typ":         tt.typ,
				"iss":         JWTIssuer,
				"aud":         JWTAudience,
				"iat":         now.Unix(),
				"exp":         now.Add(1 * time.Hour).Unix(),
				"mfa_pending": tt.mfaPending,
			}
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
			token, err := tok.SignedString([]byte(jwtTestSecret))
			require.NoError(t, err)

			parsed, err := ParseJWT(jwtTestSecret, token)
			require.NoError(t, err)
			assert.Equal(t, tt.wantType, parsed.TokenType)
			assert.Equal(t, tt.mfaPending, parsed.MFAPending)
		})
	}
}

// TestSignChallenge_Roundtrip verifies challenge token sign/parse with HS256.
func TestSignChallenge_Roundtrip(t *testing.T) {
	userID := uuid.New()
	email := "challenge@example.com"
	roles := []string{"admin"}

	token, err := SignChallenge(jwtTestSecret, userID, email, roles)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	claims, err := ParseJWT(jwtTestSecret, token)
	require.NoError(t, err)

	assert.Equal(t, userID.String(), claims.UserID)
	assert.Equal(t, email, claims.Email)
	assert.Equal(t, roles, claims.Roles)
	assert.Equal(t, "challenge", claims.TokenType)
	assert.True(t, claims.MFAPending)
}

// TestSignChallenge_EdDSA verifies EdDSA challenge signing.
func TestSignChallenge_EdDSA(t *testing.T) {
	privateKey, publicKey := ed25519TestKey()
	kid := "test-kid-challenge"

	origEdDSA := EdDSASigningKey
	EdDSASigningKey = func() (ed25519.PrivateKey, string) {
		return privateKey, kid
	}
	t.Cleanup(func() { EdDSASigningKey = origEdDSA })

	origPub := eddsaPubKey
	eddsaPubKeyMu.Lock()
	eddsaPubKey = publicKey
	eddsaPubKeyMu.Unlock()
	t.Cleanup(func() {
		eddsaPubKeyMu.Lock()
		eddsaPubKey = origPub
		eddsaPubKeyMu.Unlock()
	})

	userID := uuid.New()
	token, err := SignChallenge(jwtTestSecret, userID, "ed@x.com", nil)
	require.NoError(t, err)

	claims, err := ParseJWT(jwtTestSecret, token)
	require.NoError(t, err)
	assert.Equal(t, "challenge", claims.TokenType)
	assert.True(t, claims.MFAPending)
}

// TestParseJWT_RejectsExpiredToken verifies expiry validation.
func TestParseJWT_RejectsExpiredToken(t *testing.T) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": uuid.New().String(),
		"iss": JWTIssuer,
		"aud": JWTAudience,
		"iat": now.Add(-2 * time.Hour).Unix(),
		"exp": now.Add(-1 * time.Hour).Unix(), // expired 1 hour ago
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token, err := tok.SignedString([]byte(jwtTestSecret))
	require.NoError(t, err)

	_, err = ParseJWT(jwtTestSecret, token)
	assert.Error(t, err, "should reject expired token")
}

// TestSetEd25519PublicKey_OnceOnly verifies that only the first call to
// SetEd25519PublicKey takes effect. Subsequent calls (e.g., from a rogue
// plugin during Start) are silently ignored. This prevents JWT verification
// bypass via EdDSA public key replacement.
func TestSetEd25519PublicKey_OnceOnly(t *testing.T) {
	// Generate two distinct keypairs.
	firstPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	secondPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	// First call: must take effect.
	SetEd25519PublicKey(firstPub)

	eddsaPubKeyMu.RLock()
	assert.Equal(t, firstPub, eddsaPubKey, "first public key should be set")
	eddsaPubKeyMu.RUnlock()

	// Second call: must be silently ignored (sync.Once already fired).
	SetEd25519PublicKey(secondPub)

	eddsaPubKeyMu.RLock()
	assert.Equal(t, firstPub, eddsaPubKey, "second public key must NOT replace the first")
	eddsaPubKeyMu.RUnlock()
}

// TestSignJWT_CarriesTenantAndVersion pins the two claims a plugin-minted
// session must carry. Without the tenant, TenantHeader resolves none, which a
// multi-tenant install refuses. Without the version, the token reads as version
// zero, and any account whose version has ever been bumped rejects its own
// fresh session on the next request.
func TestSignJWT_CarriesTenantAndVersion(t *testing.T) {
	userID := uuid.New()
	token, err := SignJWT(jwtTestSecret, 3600, userID, "user@example.com", []string{"editor"}, "acme", 4)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}

	claims, err := ParseJWT(jwtTestSecret, token)
	if err != nil {
		t.Fatalf("ParseJWT: %v", err)
	}
	if claims.TenantID != "acme" {
		t.Errorf("tenant_id = %q, want %q", claims.TenantID, "acme")
	}

	var raw map[string]any
	payload := strings.Split(token, ".")[1]
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal(decoded, &raw); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, ok := raw["tv"]; !ok || got != float64(4) {
		t.Errorf("tv claim = %v (present %v), want 4", got, ok)
	}
}
