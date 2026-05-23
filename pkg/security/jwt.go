package security

import (
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// JWTIssuer is the expected "iss" claim value for all CMS-issued tokens.
	JWTIssuer = "lyeve-cms"
	// JWTAudience is the expected "aud" claim value for all CMS-issued tokens.
	JWTAudience = "lyeve-api"
)

// eddsaPubKey is the EdDSA public key for external token validation.
// Set by the runtime via SetEd25519PublicKey after auth.InitJWTSigning.
var (
	eddsaPubKey   ed25519.PublicKey
	eddsaPubKeyMu sync.RWMutex
	eddsaOnce     sync.Once
)

// SetEd25519PublicKey sets the Ed25519 public key for JWT validation.
// Call once during boot after auth.InitJWTSigning. Pass nil to disable
// EdDSA validation (HMAC-SHA256 only).
//
// Only the first call takes effect. Subsequent calls are silently ignored.
// This prevents plugins from replacing the EdDSA public key after boot.
func SetEd25519PublicKey(pub ed25519.PublicKey) {
	eddsaOnce.Do(func() {
		eddsaPubKeyMu.Lock()
		eddsaPubKey = pub
		eddsaPubKeyMu.Unlock()
	})
}

// ParseJWT validates a JWT string and returns auth claims. Supports HMAC-SHA256
// and EdDSA (when SetEd25519PublicKey has been called). Validates issuer and
// audience claims against the CMS defaults. Plugins use this
// instead of importing internal packages.
func ParseJWT(secret, tokenStr string) (*core.AuthClaims, error) {
	parser := jwt.NewParser(jwt.WithIssuer(JWTIssuer), jwt.WithAudience(JWTAudience))
	tok, err := parser.ParseWithClaims(tokenStr, &struct {
		UserID     string   `json:"sub"`
		Email      string   `json:"email,omitempty"`
		Roles      []string `json:"roles,omitempty"`
		TenantID   string   `json:"tenant_id,omitempty"`
		MFAPending bool     `json:"mfa_pending,omitempty"`
		TokenType  string   `json:"typ,omitempty"`
		jwt.RegisteredClaims
	}{}, func(t *jwt.Token) (any, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodHMAC:
			return []byte(secret), nil
		case *jwt.SigningMethodEd25519:
			eddsaPubKeyMu.RLock()
			pub := eddsaPubKey
			eddsaPubKeyMu.RUnlock()
			if pub == nil {
				return nil, errors.New("EdDSA signing not configured")
			}
			return pub, nil
		default:
			return nil, errors.New("unexpected signing method")
		}
	})
	if err != nil {
		return nil, err
	}
	c, ok := tok.Claims.(*struct {
		UserID     string   `json:"sub"`
		Email      string   `json:"email,omitempty"`
		Roles      []string `json:"roles,omitempty"`
		TenantID   string   `json:"tenant_id,omitempty"`
		MFAPending bool     `json:"mfa_pending,omitempty"`
		TokenType  string   `json:"typ,omitempty"`
		jwt.RegisteredClaims
	})
	if !ok || !tok.Valid {
		return nil, errors.New("invalid token")
	}
	return &core.AuthClaims{
		UserID:     c.UserID,
		Email:      c.Email,
		Roles:      c.Roles,
		TenantID:   c.TenantID,
		MFAPending: c.MFAPending,
		TokenType:  c.TokenType,
	}, nil
}

// SignJWT creates a signed JWT for a user session.
// Supports EdDSA (Ed25519) when configured via EdDSASigningKey bridge,
// falling back to HMAC-SHA256. Sets issuer, audience, and token-type
// claims so ParseJWT can enforce token-type discipline.
// Plugins use this instead of importing internal packages.
//
// tenantID and tokenVersion are parameters rather than defaults because a
// session that omits them is broken in two ways that do not announce
// themselves. Without the tenant claim, TenantHeader resolves no tenant, which
// on a multi-tenant install refuses the request. Without the token version,
// the session survives the account being disabled, since bumping that version
// is what revokes it.
//
// Both come from the signed-in account's own sys_users row. Prefer the host's
// SignSessionToken, which reads them for you.
func SignJWT(secret string, expirySecs int64, userID uuid.UUID, email string, roles []string, tenantID string, tokenVersion int) (string, error) {
	now := time.Now()
	claims := struct {
		UserID       string   `json:"sub"`
		Email        string   `json:"email,omitempty"`
		Roles        []string `json:"roles,omitempty"`
		TenantID     string   `json:"tenant_id,omitempty"`
		TokenVersion int      `json:"tv,omitempty"`
		MFAPending   bool     `json:"mfa_pending,omitempty"`
		TokenType    string   `json:"typ,omitempty"`
		jwt.RegisteredClaims
	}{
		UserID:       userID.String(),
		Email:        email,
		Roles:        roles,
		TenantID:     tenantID,
		TokenVersion: tokenVersion,
		TokenType:    "session",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(expirySecs) * time.Second)),
			Issuer:    JWTIssuer,
			Audience:  jwt.ClaimStrings{JWTAudience},
		},
	}

	// EdDSA bridge: use Ed25519 when the signing key is wired.
	if b := EdDSASigningKey; b != nil {
		if sk, kid := b(); sk != nil && kid != "" {
			tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
			tok.Header["kid"] = kid
			return tok.SignedString(sk)
		}
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}
