package auth

import (
	"context"
	"crypto/subtle"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// idTokenParser pins the signature algorithm set and requires an expiry.
//
// Only asymmetric algorithms are listed. An ID token is verified with the
// provider's public key, so admitting an HMAC algorithm would let a caller
// sign a token using that public key as the shared secret. "none" is refused
// by the same list.
//
// Issuer and audience vary per provider, so they are checked after parsing
// rather than baked in here.
var idTokenParser = jwt.NewParser(
	jwt.WithValidMethods([]string{
		"RS256", "RS384", "RS512",
		"PS256", "PS384", "PS512",
		"ES256", "ES384", "ES512",
	}),
	jwt.WithExpirationRequired(),
)

// ParseIDToken validates an OIDC ID token against the issuing provider's JWKS
// and returns its claims.
//
// This is deliberately not ParseExternal. That function validates an API token
// addressed to this engine: it requires the audience to be JWTAudience and it
// decodes into the engine's own Claims. An ID token is addressed to the OAuth
// client, carries the provider's claim set, and must also match the nonce
// minted at login. Sharing a parser between the two would mean accepting a
// token issued for a different audience.
//
// The checks follow OIDC Core 1.0 section 3.1.3.7. The caller is responsible
// for SSRF-validating jwksURL before this is reached.
func ParseIDToken(ctx context.Context, idToken, jwksURL, expectedIssuer, clientID, nonce string) (map[string]any, error) {
	if clientID == "" {
		return nil, fmt.Errorf("id token: no client id configured")
	}
	if expectedIssuer == "" {
		return nil, fmt.Errorf("id token: no issuer configured")
	}

	keys, err := fetchJWKS(ctx, jwksURL)
	if err != nil {
		return nil, fmt.Errorf("id token: %w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("id token: provider published no usable signing keys")
	}

	claims := jwt.MapClaims{}
	tok, err := idTokenParser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if key, ok := keys[kid]; ok {
			return key, nil
		}
		// Same rule as ParseExternal: a single-key set may legitimately omit
		// kid, but an unknown kid against a multi-key set must be refused
		// rather than guessed. Map iteration would pick a key at random.
		if len(keys) == 1 {
			for _, k := range keys {
				return k, nil
			}
		}
		return nil, fmt.Errorf("no matching key for kid %q", kid)
	})
	if err != nil {
		return nil, fmt.Errorf("id token: parse: %w", err)
	}
	if !tok.Valid {
		return nil, fmt.Errorf("id token: invalid")
	}

	iss, err := claims.GetIssuer()
	if err != nil {
		return nil, fmt.Errorf("id token: issuer: %w", err)
	}
	if iss != expectedIssuer {
		return nil, fmt.Errorf("id token: expected issuer %q, got %q", expectedIssuer, iss)
	}

	aud, err := claims.GetAudience()
	if err != nil {
		return nil, fmt.Errorf("id token: audience: %w", err)
	}
	if !containsString(aud, clientID) {
		return nil, fmt.Errorf("id token: audience does not include this client")
	}
	// With more than one audience the token is shared with another party, so
	// azp must name the party it was actually minted for.
	if len(aud) > 1 {
		azp, _ := claims["azp"].(string)
		if azp == "" {
			return nil, fmt.Errorf("id token: multiple audiences and no azp claim")
		}
		if azp != clientID {
			return nil, fmt.Errorf("id token: azp names a different client")
		}
	}

	// The nonce binds the token to the login that started this flow. It is
	// compared whenever one was minted. A provider that drops it fails here
	// rather than silently losing replay protection.
	if nonce != "" {
		got, _ := claims["nonce"].(string)
		if got == "" {
			return nil, fmt.Errorf("id token: no nonce claim")
		}
		if !constantTimeEqual(got, nonce) {
			return nil, fmt.Errorf("id token: nonce mismatch")
		}
	}

	if _, err := claims.GetIssuedAt(); err != nil {
		return nil, fmt.Errorf("id token: issued-at: %w", err)
	}

	return map[string]any(claims), nil
}

// constantTimeEqual compares two nonces without leaking their contents
// through comparison time.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
