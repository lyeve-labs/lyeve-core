package auth_test

import (
	"context"
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

func TestJWT_EdgeCases(t *testing.T) {
	sec := "edge-case-test-secret-32bytes!"

	t.Run("expired_token_rejected", func(t *testing.T) {
		tok, _ := auth.Sign(sec, -1, uuid.New(), "exp@t.com", nil, "", 1)
		_, err := auth.Parse(sec, tok)
		assert.True(t, errors.Is(err, jwt.ErrTokenExpired))
	})

	t.Run("invalid_signature_rejected", func(t *testing.T) {
		tok, _ := auth.Sign(sec, 3600, uuid.New(), "sig@t.com", nil, "", 1)
		_, err := auth.Parse(sec, tok[:len(tok)-3]+"bad")
		assert.True(t, errors.Is(err, jwt.ErrTokenSignatureInvalid))
	})

	t.Run("future_nbf_rejected", func(t *testing.T) {
		now := time.Now()
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
			UserID: uuid.New().String(),
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: uuid.New().String(), Issuer: security.JWTIssuer,
				IssuedAt: jwt.NewNumericDate(now), Audience: jwt.ClaimStrings{security.JWTAudience},
				ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Hour)),
				NotBefore: jwt.NewNumericDate(now.Add(time.Hour)),
			},
		}).SignedString([]byte(sec))
		_, err := auth.Parse(sec, tok)
		assert.True(t, errors.Is(err, jwt.ErrTokenNotValidYet))
	})

	t.Run("wrong_alg_es256_rejected", func(t *testing.T) {
		now := time.Now()
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.Claims{
			UserID: uuid.New().String(),
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: uuid.New().String(), Issuer: security.JWTIssuer,
				IssuedAt: jwt.NewNumericDate(now), Audience: jwt.ClaimStrings{security.JWTAudience},
				ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			},
		})
		tok.Header["alg"] = "ES256"
		s, _ := tok.SignedString([]byte(sec))
		_, err := auth.Parse(sec, s)
		assert.Error(t, err, "ES256 alg must be rejected")
	})
}
func TestEdDSA_MissingKIDHeader(t *testing.T) {
	if !auth.IsEdDSAActive() {
		t.Skip("EdDSA not active")
	}
	now := time.Now()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodEdDSA, auth.Claims{
		UserID: uuid.New().String(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.New().String(), Issuer: security.JWTIssuer,
			IssuedAt: jwt.NewNumericDate(now), Audience: jwt.ClaimStrings{security.JWTAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
	}).SignedString(priv)
	_, err = auth.Parse("fallback", tok)
	assert.Error(t, err, "EdDSA token from unknown key must be rejected")
}
func TestRefresh_ConcurrentRotate(t *testing.T) {
	s := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-edge")
	ctx := context.Background()
	issued, err := s.Issue(ctx, "u1", 5*time.Minute)
	require.NoError(t, err)
	start, ch := make(chan struct{}), make(chan error, 2)
	for range 2 {
		go func() { <-start; _, err := s.Rotate(ctx, issued.RefreshToken, 5*time.Minute); ch <- err }()
	}
	close(start)
	ok := 0
	for range 2 {
		if <-ch == nil {
			ok++
		}
	}
	assert.Equal(t, 1, ok)
}
func TestRefresh_ReuseAfterRotation(t *testing.T) {
	s := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-edge")
	ctx := context.Background()
	issued, err := s.Issue(ctx, "u1", 5*time.Minute)
	require.NoError(t, err)
	r1, err := s.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
	_, err = s.Rotate(ctx, r1.RefreshToken, 5*time.Minute)
	require.NoError(t, err)
	_, err = s.Rotate(ctx, issued.RefreshToken, 5*time.Minute)
	assert.True(t, errors.Is(err, auth.ErrTokenReuse))
}
