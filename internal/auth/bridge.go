// Package auth wires internal/auth's ParseMulti into security.VerifyJWT so
// plugins can validate JWTs through the stable public API without
// importing internal packages or hand-rolling verification.
//
// The function variable pattern breaks the import cycle:
//
//	core <- internal/auth (ok: established direction)
//	core -> internal/auth (would be a cycle, bridge avoids it)
package auth

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

var errNoSecrets = errors.New("lyeve-core: no JWT secrets configured")

func init() {
	security.VerifyJWT = func(host core.Host, tokenStr string) (*core.AuthClaims, error) {
		// core.SecretList is the one place that knows the precedence:
		// SecretsProvider where the host implements it, Config otherwise.
		// Reading it here keeps a host that publishes the rotation list
		// through Config visible to this bridge.
		secrets := core.SecretList(host, "jwt_secrets")
		if len(secrets) == 0 {
			if s := core.Secret(host, "jwt_secret"); s != "" {
				secrets = []string{s}
			}
		}
		if len(secrets) == 0 {
			return nil, errNoSecrets
		}
		claims, err := ParseMulti(secrets, tokenStr)
		if err != nil {
			return nil, err
		}
		return claims.AuthClaims(), nil
	}

	security.EdDSASigningKey = func() (ed25519.PrivateKey, string) {
		if !IsEdDSAActive() {
			return nil, ""
		}
		signingKeyMu.RLock()
		key := signingKey
		kid := signingKid
		signingKeyMu.RUnlock()
		return key, kid
	}

	security.ValidateIDToken = ParseIDToken

	security.JWKSLiveProbe = func() error {
		if !IsEdDSAActive() {
			return fmt.Errorf("EdDSA is not active; JWT signing may be using HMAC-SHA256 only")
		}
		pub := PublicKey()
		if pub == nil {
			return fmt.Errorf("no Ed25519 public key loaded")
		}
		if len(pub) != ed25519.PublicKeySize {
			return fmt.Errorf("Ed25519 public key is wrong length: %d bytes (expected %d)", len(pub), ed25519.PublicKeySize)
		}
		return nil
	}
}
