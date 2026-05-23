package security

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// GenerateNonce returns a random 32-byte nonce encoded as base64url, suitable
// for OIDC ID token replay protection.
func GenerateNonce() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// SignChallenge creates a short-lived MFA challenge token.
// Supports EdDSA (Ed25519) when configured via EdDSASigningKey bridge,
// falling back to HMAC-SHA256. The token carries the user identity and an
// "mfa_pending" claim so downstream middleware blocks full session access
// until the second factor is verified.
func SignChallenge(secret string, userID uuid.UUID, email string, roles []string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":         userID.String(),
		"email":       email,
		"roles":       roles,
		"iat":         now.Unix(),
		"exp":         now.Add(5 * time.Minute).Unix(),
		"jti":         uuid.New().String(),
		"iss":         "lyeve-cms",
		"aud":         "lyeve-api",
		"typ":         "challenge",
		"mfa_pending": true,
	}

	// EdDSA bridge: use Ed25519 when the signing key is wired.
	if b := EdDSASigningKey; b != nil {
		if sk, kid := b(); sk != nil && kid != "" {
			claims["kid"] = kid
			tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			tok.Header["kid"] = kid
			return tok.SignedString(sk)
		}
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// SignMap signs a jwt.MapClaims payload with the given HMAC-SHA256 secret.
// EdDSA-aware: when an Ed25519 private key is registered (via the func-var
// bridge in internal/auth), tokens are signed with Ed25519 instead.
func SignMap(secret string, claims jwt.MapClaims) (string, error) {
	var method jwt.SigningMethod = jwt.SigningMethodHS256
	var key any = []byte(secret)

	if b := EdDSASigningKey; b != nil {
		if sk, kid := b(); sk != nil && kid != "" {
			method = jwt.SigningMethodEdDSA
			key = sk
			if claims["kid"] == nil {
				claims["kid"] = kid
			}
		}
	}

	tok := jwt.NewWithClaims(method, claims)
	return tok.SignedString(key)
}

// ParseMap validates and decodes a JWT token against the given secret.
// Supports HMAC-SHA256 and EdDSA (Ed25519, when SetEd25519PublicKey has
// been called). Plugins use this instead of importing internal packages.
func ParseMap(secret string, tokenStr string) (jwt.MapClaims, error) {
	tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodHMAC:
			return []byte(secret), nil
		case *jwt.SigningMethodEd25519:
			eddsaPubKeyMu.RLock()
			pub := eddsaPubKey
			eddsaPubKeyMu.RUnlock()
			if pub == nil {
				return nil, fmt.Errorf("EdDSA signing not configured")
			}
			return pub, nil
		default:
			return nil, fmt.Errorf("unexpected signing method: %v", t.Method.Alg())
		}
	})
	if err != nil {
		return nil, err
	}
	if claims, ok := tok.Claims.(jwt.MapClaims); ok && tok.Valid {
		return claims, nil
	}
	return nil, fmt.Errorf("invalid token claims")
}

// ExtractEmailVerified checks whether the "email_verified" claim is present
// and true in the given OIDC userinfo or ID token claims map. Returns false
// when the claim is absent or false: callers MUST reject logins from
// unverified email addresses.
func ExtractEmailVerified(claims map[string]any) bool {
	v, ok := claims["email_verified"]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

// ValidateIDToken validates an OIDC ID token against the provider's JWKS
// endpoint, issuer, client_id, and nonce. Returns the parsed claims map.
// Bound at init time by internal/auth, alongside the other func vars here.
// Nil when auth is not linked in, which is the case in a plugin's own test
// binary. Callers must guard against nil.
var ValidateIDToken func(ctx context.Context, idToken, jwksURI, issuer, clientID, nonce string) (map[string]any, error)

// EdDSASigningKey returns the active Ed25519 private key and its kid for
// JWT signing. Returns (nil, "") when EdDSA is not active.
// Set at init time by internal/auth via the func-var bridge.
var EdDSASigningKey func() (ed25519.PrivateKey, string)
