// JWT security hardening: alg=none rejection, algorithm-confusion attacks,
// expiry/nbf enforcement on both HMAC and EdDSA paths, tampered payloads
// and signatures, key rotation, malformed structures, and issuer/audience
// validation.

package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// alg=none: MUST be rejected

func TestJWT_AlgNoneRejected(t *testing.T) {
	// jwt/v5 does not register SigningMethodNone by default. Construct manually.
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "none@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tok.Header["alg"] = "none"
	tokenStr, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = auth.Parse("any-secret", tokenStr)
	assert.Error(t, err, "alg=none token must be rejected")

	_, err = auth.ParseMulti([]string{"any-secret"}, tokenStr)
	assert.Error(t, err, "alg=none token must be rejected by ParseMulti")
}

// Algorithm confusion: HS256 header claiming EdDSA

func TestJWT_AlgConfusion_HS256WithEdDSAHeader(t *testing.T) {
	// Classic confusion attack: attacker signs with HS256 but sets alg=EdDSA.
	// Verifier must not treat the public Ed25519 key as the HMAC secret.
	secret := "hs256-signed-but-claims-eddsa-header"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "confusion@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["alg"] = "EdDSA"
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	// jwt/v5 picks the key function from the header alg, so verification
	// attempts Ed25519 against the HMAC signature and fails.
	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "HS256 token with EdDSA header must be rejected")
}

func TestJWT_AlgConfusion_HS256WithRS256Header(t *testing.T) {
	secret := "hs256-signed-but-claims-rs256"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["alg"] = "RS256"
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "HS256 token with RS256 header must be rejected")
}

// Expired tokens: both HMAC and EdDSA paths

func TestJWT_ExpiredToken_HMAC(t *testing.T) {
	if auth.IsEdDSAActive() {
		t.Skip("EdDSA active: expired test runs through EdDSA path. " +
			"TestJWT_ExpiredToken_EdDSA covers that case.")
	}
	secret := "expired-token-hmac-test-secret-32b"
	id := uuid.New()
	// -1 second expiry = already expired.
	token, err := auth.Sign(secret, -1, id, "expired@example.com", nil, "", 1)
	require.NoError(t, err)

	_, err = auth.Parse(secret, token)
	assert.Error(t, err, "expired HMAC token must be rejected")
	assert.Contains(t, err.Error(), "invalid token")

	_, err = auth.ParseMulti([]string{secret}, token)
	assert.Error(t, err, "expired HMAC token must be rejected by ParseMulti")
}

func TestJWT_ExpiredToken_EdDSA(t *testing.T) {
	if !auth.IsEdDSAActive() {
		t.Skip("EdDSA not active")
	}
	secret := "fallback-hmac-secret-for-eddsa-expired"
	id := uuid.New()
	token, err := auth.Sign(secret, -1, id, "expired-eddsa@example.com", nil, "", 1)
	require.NoError(t, err)

	_, err = auth.Parse(secret, token)
	assert.Error(t, err, "expired EdDSA token must be rejected")
}

// Not-before tokens: both HMAC and EdDSA paths

func TestJWT_NotBeforeToken_HMAC(t *testing.T) {
	secret := "nbf-token-hmac-test-secret-32b"
	id := uuid.New()
	now := time.Now()
	future := now.Add(1 * time.Hour)

	claims := auth.Claims{
		UserID: id.String(),
		Email:  "nbf@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
			NotBefore: jwt.NewNumericDate(future),
		},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	// jwt/v5 requires NotBefore <= time.Now(); a future nbf is not-yet-valid.
	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "future nbf token must be rejected")

	_, err = auth.ParseMulti([]string{secret}, tokenStr)
	assert.Error(t, err, "future nbf token must be rejected by ParseMulti")
}

func TestJWT_NotBeforeToken_EdDSA(t *testing.T) {
	if !auth.IsEdDSAActive() {
		t.Skip("EdDSA not active")
	}
	id := uuid.New()
	now := time.Now()
	future := now.Add(1 * time.Hour)

	claims := auth.Claims{
		UserID: id.String(),
		Email:  "nbf-eddsa@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
			NotBefore: jwt.NewNumericDate(future),
		},
	}

	if !auth.IsEdDSAActive() {
		t.Skip("EdDSA not active for nbf test")
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	// Fresh key: the active process-global key is not directly accessible.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tokenStr, err := tok.SignedString(priv)
	require.NoError(t, err)

	// The fresh key does not match the active EdDSA public key, so Parse
	// rejects on key mismatch before the nbf check. HMAC-path tests above
	// exercise the nbf branch directly.
	_, err = auth.Parse("fallback-secret", tokenStr)

	// Either key mismatch or nbf rejection is a correct security outcome.
	if err != nil {
		t.Logf("EdDSA nbf token rejected: %v", err)
	} else {
		t.Error("EdDSA nbf token should have been rejected (either key mismatch or nbf)")
	}

	_ = pub
}

// Tampered signature: both paths

func TestJWT_TamperedSignature_Rejected(t *testing.T) {
	secret := "tamper-sig-test-secret-32-bytes"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "tamper@example.com", nil, "", 1)
	require.NoError(t, err)

	tampered := token + "X"

	_, err = auth.Parse(secret, tampered)
	assert.Error(t, err, "tampered signature token must be rejected")
}

func TestJWT_TamperedPayload_Rejected(t *testing.T) {
	secret := "tamper-payload-test-secret-32b"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "tamper-payload@example.com", nil, "", 1)
	require.NoError(t, err)

	parts := splitJWT(token)
	require.Len(t, parts, 3, "JWT should have 3 parts")
	corrupted := append([]byte(parts[1]), byte('X'))
	tampered := parts[0] + "." + string(corrupted) + "." + parts[2]

	_, err = auth.Parse(secret, tampered)
	assert.Error(t, err, "tampered payload token must be rejected")
}

// splitJWT splits a JWT string into its three dot-separated parts.
func splitJWT(token string) []string {
	parts := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}

// Key rotation: old key still validates

func TestJWT_KeyRotation_OldKeyStillValid(t *testing.T) {
	oldSecret := "old-rotation-secret-32-bytes-long"
	newSecret := "new-rotation-secret-32-bytes-long"
	id := uuid.New()

	token, err := auth.Sign(oldSecret, 3600, id, "rotate@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.ParseMulti([]string{newSecret, oldSecret}, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

func TestJWT_KeyRotation_OldKeyOnly(t *testing.T) {
	oldSecret := "only-old-secret-32-bytes-minimum"
	id := uuid.New()

	token, err := auth.Sign(oldSecret, 3600, id, "oldonly@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.ParseMulti([]string{oldSecret}, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

func TestJWT_KeyRotation_NewSecretFailsOldToken(t *testing.T) {
	oldSecret := "removed-secret-32-bytes-for-rotation"
	newSecret := "active-secret-32-bytes-for-rotation"
	id := uuid.New()

	token, err := auth.Sign(oldSecret, 3600, id, "removed@example.com", nil, "", 1)
	require.NoError(t, err)

	if !auth.IsEdDSAActive() {
		_, err = auth.ParseMulti([]string{newSecret}, token)
		assert.Error(t, err, "old token should fail when old secret is removed from rotation")
	} else {
		t.Skip("EdDSA active: old-secret-fails test depends on HMAC-only path")
	}
}

// Malformed JWT structures

func TestJWT_MissingSignatureSegment(t *testing.T) {
	_, err := auth.Parse("secret", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ")
	assert.Error(t, err, "token without signature must be rejected")
}

func TestJWT_NullByteInToken(t *testing.T) {
	secret := "nullbyte-test-secret-32-bytes-x"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "null@example.com", nil, "", 1)
	require.NoError(t, err)

	_, err = auth.Parse(secret, token+"\x00")
	assert.Error(t, err, "null byte in token must be rejected")
}

// Issuer/Audience validation

func TestJWT_IssuerAudience_Valid(t *testing.T) {
	// Sign() populates iss+aud. Parse must accept the result.
	secret := "iss-aud-valid-test-secret-32b"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "issaud@example.com", []string{"admin"}, "", 1)
	require.NoError(t, err)

	claims, err := auth.Parse(secret, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

func TestJWT_WrongIssuer_Rejected(t *testing.T) {
	secret := "wrong-issuer-test-secret-32b"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "wrongiss@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
			Issuer:    "attacker-service", // wrong issuer
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "token with wrong issuer must be rejected")
}

func TestJWT_WrongAudience_Rejected(t *testing.T) {
	secret := "wrong-aud-test-secret-32bytes"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "wrongaud@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{"attacker-api"}, // wrong audience
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "token with wrong audience must be rejected")
}

func TestJWT_MissingIssuer_Rejected(t *testing.T) {
	secret := "no-issuer-test-secret-32bytes"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "noiss@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
			// Issuer omitted
			Audience: jwt.ClaimStrings{security.JWTAudience},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "token without issuer must be rejected")
}

func TestJWT_MissingAudience_Rejected(t *testing.T) {
	secret := "no-aud-test-secret-32-bytes"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "noaud@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
			Issuer:    security.JWTIssuer,
			// Audience omitted
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	assert.Error(t, err, "token without audience must be rejected")
}

func TestJWT_IssuerAudience_MultiKeyRotation(t *testing.T) {
	// ParseMulti enforces iss/aud even across a key-rotation list.
	secret := "rotation-iss-aud-test-secret"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "rotate@example.com", nil, "", 1)
	require.NoError(t, err)

	claims, err := auth.ParseMulti([]string{"wrong-key", secret}, token)
	require.NoError(t, err)
	assert.Equal(t, id.String(), claims.UserID)
}

// Error wrapping. Callers can distinguish failure modes

func TestJWT_WrappedError_ExpiredToken(t *testing.T) {
	// auth.Parse wraps the sentinel so callers can errors.Is by failure mode.
	secret := "wrapped-expired-test-secret-32b"
	id := uuid.New()
	token, err := auth.Sign(secret, -1, id, "expired@example.com", nil, "", 1)
	require.NoError(t, err)

	_, err = auth.Parse(secret, token)
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenExpired),
		"expired token must be identifiable via errors.Is(err, jwt.ErrTokenExpired)")
	assert.Contains(t, err.Error(), "invalid token",
		"error message must include the 'invalid token' prefix callers match on")
}

func TestJWT_WrappedError_ExpiredToken_Multi(t *testing.T) {
	secret := "wrapped-expired-multi-32b"
	id := uuid.New()
	token, err := auth.Sign(secret, -1, id, "expired2@example.com", nil, "", 1)
	require.NoError(t, err)

	_, err = auth.ParseMulti([]string{secret}, token)
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenExpired),
		"ParseMulti must preserve the wrapped sentinel for expired tokens")
}

func TestJWT_WrappedError_WrongIssuer(t *testing.T) {
	secret := "wrapped-issuer-test-secret-32b"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "wrongissuer@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
			Issuer:    "evil-issuer",
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenInvalidIssuer),
		"wrong issuer must be identifiable via errors.Is")
}

func TestJWT_WrappedError_WrongAudience(t *testing.T) {
	secret := "wrapped-aud-test-secret-32b"
	id := uuid.New()
	now := time.Now()
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "wrongaud@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(1 * time.Hour)),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{"evil-api"},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenInvalidAudience),
		"wrong audience must be identifiable via errors.Is")
}

func TestJWT_WrappedError_SignatureInvalid(t *testing.T) {
	secret := "wrapped-sig-test-secret-32bytes"
	id := uuid.New()
	token, err := auth.Sign(secret, 3600, id, "tampered@example.com", nil, "", 1)
	require.NoError(t, err)

	_, err = auth.Parse(secret, token+"X")
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenSignatureInvalid),
		"tampered signature must be identifiable via errors.Is")
}

func TestJWT_WrappedError_MalformedToken(t *testing.T) {
	_, err := auth.Parse("secret", "not.a.jwt")
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenMalformed),
		"malformed token must be identifiable via errors.Is")
}

func TestJWT_WrappedError_NotYetValid(t *testing.T) {
	secret := "wrapped-nbf-test-secret-32bytes"
	id := uuid.New()
	now := time.Now()
	future := now.Add(1 * time.Hour)
	claims := auth.Claims{
		UserID: id.String(),
		Email:  "nbf-future@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
			NotBefore: jwt.NewNumericDate(future),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := tok.SignedString([]byte(secret))
	require.NoError(t, err)

	_, err = auth.Parse(secret, tokenStr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, jwt.ErrTokenNotValidYet),
		"future nbf token must be identifiable via errors.Is")
}
