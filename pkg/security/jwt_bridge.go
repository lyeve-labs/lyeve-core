package security

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// VerifyJWT validates a JWT string against the CMS's configured signing keys
// and returns the parsed claims. It supports EdDSA (Ed25519) and HMAC-SHA256
// algorithms, key rotation via the jwt_secrets config list, and proper
// claim validation (exp, nbf, alg).
//
// Secrets are sourced from the host config: jwt_secrets (comma-separated
// rotation list) or jwt_secret (a single key). When both are present,
// jwt_secrets takes precedence. Plugins that need to validate tokens
// from WebSocket connection_init or other non-HTTP contexts use this
// instead of hand-rolling JWT verification.
//
// Security guarantees:
//   - alg=none tokens are rejected (require valid algorithm header)
//   - HS256 signed tokens validated with HMAC comparison
//   - EdDSA tokens validated with the active Ed25519 public key
//   - Expired and not-before tokens are rejected with descriptive errors
//   - Key rotation: tokens signed with any secret in the rotation list
//     are accepted (newest first for zero-downtime rollover)
//
// VerifyJWT is set at init time by internal/auth to break the import cycle.
// It is nil when auth is not initialized. Callers must guard against nil.
var VerifyJWT func(host core.Host, tokenStr string) (*core.AuthClaims, error)

// JWKSLiveProbe returns nil when the JWKS key store is healthy (EdDSA is
// active and a valid public key is available for JWT verification). It
// returns a descriptive error when JWKS is not live: terse enough for
// preflight messages, but specific enough to diagnose misconfiguration.
//
// Set at init time by internal/auth via the same bridge pattern as
// VerifyJWT. Nil when auth is not initialized. Callers must guard against nil.
var JWKSLiveProbe func() error
