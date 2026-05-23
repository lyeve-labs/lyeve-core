package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// Invalid-token errors wrap a sentinel that errors.Is can match, not a bare
// errors.New("invalid token").

func TestAuthJWT_InvalidTokenErrorWrappedNotBareNew(t *testing.T) {
	secret := "wrapped-error-test-secret-32bytes!"

	t.Run("expired", func(t *testing.T) {
		id := uuid.New()
		token, err := auth.Sign(secret, -1, id, "expired@test.com", nil, "", 1)
		require.NoError(t, err)

		_, err = auth.Parse(secret, token)
		require.Error(t, err)
		assert.True(t, errors.Is(err, jwt.ErrTokenExpired),
			"expired token error must wrap jwt.ErrTokenExpired so callers can errors.Is")
		assert.Contains(t, err.Error(), "invalid token",
			"human-readable prefix must still be present")
	})

	// NOTE: HMAC-only. EdDSA ignores the secret parameter.
	t.Run("wrong_secret", func(t *testing.T) {
		if auth.IsEdDSAActive() {
			t.Skip("wrong_secret is HMAC-only; EdDSA ignores the secret parameter")
		}
		id := uuid.New()
		token, err := auth.Sign(secret, 3600, id, "user@test.com", nil, "", 1)
		require.NoError(t, err)

		_, err = auth.Parse("different-secret-32bytes!!!!!!!!", token)
		require.Error(t, err)
		assert.True(t, errors.Is(err, jwt.ErrTokenSignatureInvalid),
			"wrong-secret error must wrap jwt.ErrTokenSignatureInvalid")
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := auth.Parse(secret, "not-a-valid-jwt-string")
		require.Error(t, err)
		assert.True(t, errors.Is(err, jwt.ErrTokenMalformed),
			"malformed token error must wrap jwt.ErrTokenMalformed")
	})
}

// Parse rejects an expired token, alg=none and a tampered signature. A valid
// token returns its claims.

func TestAuthJWT_ParseRejectsExpiredAndNoneAlg(t *testing.T) {
	secret := "parse-reject-test-secret-32bytes!"

	t.Run("rejects_expired", func(t *testing.T) {
		id := uuid.New()
		token, err := auth.Sign(secret, -1, id, "expired@test.com", []string{"user"}, "", 1)
		require.NoError(t, err)

		_, err = auth.Parse(secret, token)
		assert.Error(t, err)
	})

	t.Run("rejects_alg_none", func(t *testing.T) {
		id := uuid.New()
		now := time.Now()
		claims := auth.Claims{
			UserID: id.String(),
			Email:  "none@test.com",
			RegisteredClaims: jwt.RegisteredClaims{
				Subject:   id.String(),
				IssuedAt:  jwt.NewNumericDate(now),
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
		tok.Header["alg"] = "none"
		tokenStr, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		require.NoError(t, err)

		_, err = auth.Parse(secret, tokenStr)
		assert.Error(t, err, "alg=none token must be rejected")
	})

	t.Run("rejects_tampered_signature", func(t *testing.T) {
		if auth.IsEdDSAActive() {
			t.Skip("tampered-signature test is HMAC-only; EdDSA uses public-key validation")
		}
		id := uuid.New()
		token, err := auth.Sign(secret, 3600, id, "tampered@test.com", nil, "", 1)
		require.NoError(t, err)

		_, err = auth.Parse(secret, token[:len(token)-1]+"X")
		assert.Error(t, err, "tampered signature must be rejected")
	})

	t.Run("valid_token_returns_claims", func(t *testing.T) {
		id := uuid.New()
		token, err := auth.Sign(secret, 3600, id, "valid@test.com", []string{"admin", "user"}, "", 1)
		require.NoError(t, err)

		claims, err := auth.Parse(secret, token)
		require.NoError(t, err)
		assert.Equal(t, id.String(), claims.UserID)
		assert.Equal(t, "valid@test.com", claims.Email)
		assert.Equal(t, []string{"admin", "user"}, claims.Roles)
		// No tenant was given, so the token names none.
		if claims.TenantID != "" {
			t.Errorf("JWT has unexpected TenantID=%q: a token signed without a tenant must not name one", claims.TenantID)
		}
	})
}
