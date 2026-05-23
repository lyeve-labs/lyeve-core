package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

type contextKey string

// ClaimsKey is the context key under which *Claims is stored by the JWT
// authentication middleware.
const ClaimsKey contextKey = "claims"

// defaultParser is a pre-built jwt.Parser used by Parse and ParseMulti.
// Building it once at init avoids re-parsing the same options on every call.
var defaultParser = jwt.NewParser(
	jwt.WithIssuer(security.JWTIssuer),
	jwt.WithAudience(security.JWTAudience),
	jwt.WithValidMethods([]string{"EdDSA", "HS256"}),
	jwt.WithExpirationRequired(),
)

// audienceOnlyParser validates the audience claim without requiring a
// specific issuer. Used by ParseExternal, where the issuer is dynamic
// (per-OIDC-provider) but the audience is always "lyeve-api". The calling
// code verifies the issuer from the decoded claims after parsing, avoiding
// a per-request jwt.NewParser allocation. Expiry is required: the parser
// checks exp only when a token carries one, and a token from another issuer
// without it would never expire here.
var audienceOnlyParser = jwt.NewParser(jwt.WithAudience(security.JWTAudience), jwt.WithExpirationRequired())

// Claims is the JWT payload stored in every token.
type Claims struct {
	UserID       string   `json:"sub"`
	Email        string   `json:"email"`
	Roles        []string `json:"roles"`
	TenantID     string   `json:"tenant_id,omitempty"`
	TokenType    string   `json:"typ,omitempty"`
	TokenVersion int      `json:"tv"`
	MFAPending   bool     `json:"mfa_pending,omitempty"`
	jwt.RegisteredClaims
}

// HasRole returns true if the claims include the given role.
func (c *Claims) HasRole(role string) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AuthClaims converts the internal JWT Claims to a core.AuthClaims
// value suitable for use with plugin-context helpers and middleware.
func (c *Claims) AuthClaims() *core.AuthClaims {
	if c == nil {
		return nil
	}
	return &core.AuthClaims{
		UserID:     c.UserID,
		Email:      c.Email,
		Roles:      c.Roles,
		TenantID:   c.TenantID,
		MFAPending: c.MFAPending,
	}
}

// SignChallenge creates a short-lived MFA challenge token (5 minutes).
// Uses EdDSA when active, falling back to HMAC-SHA256.
// Each challenge token carries a unique jti, so the MFAVerify handler can
// refuse a replayed one.
func SignChallenge(secret string, userID uuid.UUID, email string, roles []string, tenantID string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:     userID.String(),
		Email:      email,
		Roles:      roles,
		TenantID:   tenantID,
		MFAPending: true,
		TokenType:  "challenge",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.New().String(),
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}

	if IsEdDSAActive() {
		signingKeyMu.RLock()
		key := signingKey
		kid := signingKid
		signingKeyMu.RUnlock()
		if key != nil {
			tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			if kid != "" {
				tok.Header["kid"] = kid
			}
			return tok.SignedString(key)
		}
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// Sign creates a signed JWT string.
// When EdDSA is active (IsEdDSAActive()), the token is signed with the
// Ed25519 private key and carries an "EdDSA" alg header. Otherwise,
// legacy HMAC-SHA256 signing is used.
// tokenVersion is embedded as the "tv" claim and validated by the
// tokenVersionCheck middleware on each authenticated request. Pass 0
// for challenge tokens and other short-lived credentials that should
// not be subject to version-scoped invalidation.
//
// tenantID becomes the "tenant_id" claim, which is how TenantHeader learns
// which tenant a request belongs to. It is omitted when empty, so a
// super_admin with no home tenant carries no tenant claim.
func Sign(secret string, expirySecs int64, userID uuid.UUID, email string, roles []string, tenantID string, tokenVersion int) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:       userID.String(),
		Email:        email,
		Roles:        roles,
		TenantID:     tenantID,
		TokenVersion: tokenVersion,
		TokenType:    "session",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.New().String(),
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(expirySecs) * time.Second)),
			Issuer:    security.JWTIssuer,
			Audience:  jwt.ClaimStrings{security.JWTAudience},
		},
	}

	if IsEdDSAActive() {
		signingKeyMu.RLock()
		key := signingKey
		kid := signingKid
		signingKeyMu.RUnlock()
		if key != nil {
			tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			if kid != "" {
				tok.Header["kid"] = kid
			}
			return tok.SignedString(key)
		}
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// Parse validates a JWT string and returns the claims.
//
// One algorithm is accepted per install, and it is the one the install is
// configured for. With EdDSA active that is EdDSA, verified against the
// Ed25519 public key. Otherwise it is HMAC-SHA256 against the provided
// secret.
func Parse(secret, tokenStr string) (*Claims, error) {
	tok, err := defaultParser.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if IsEdDSAActive() {
			if t.Header["alg"] != "EdDSA" {
				// The configured algorithm decides what verifies. The
				// token's header cannot choose another one, which rules out
				// algorithm confusion.
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			pub := PublicKey()
			if pub == nil {
				return nil, fmt.Errorf("EdDSA token but no public key available")
			}
			return pub, nil
		}
		// HMAC is the verifying key only where it is the configured one,
		// which is JWT_ALG=HS256 or a key path the engine could not read.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		if secret == "" {
			return nil, errors.New("HMAC token rejected: no signing secret configured")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, errors.New("invalid token claims")
	}
	return claims, nil
}

// ParseMulti tries each secret in order and returns claims from the first
// successful validation. Use for zero-downtime key rotation: list the new
// secret first, old secret(s) last.
func ParseMulti(secrets []string, tokenStr string) (*Claims, error) {
	var lastErr error
	for _, s := range secrets {
		claims, err := Parse(s, tokenStr)
		if err == nil {
			return claims, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no secrets provided")
}
