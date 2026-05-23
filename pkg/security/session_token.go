package security

import (
	"context"

	"github.com/google/uuid"
)

// SessionTokenSigner is an optional interface that engine hosts implement
// to sign session JWTs without exposing the signing secret to the caller.
// Plugins that issue session tokens type-assert the Host to this interface,
// so the signing secret never leaves the host.
type SessionTokenSigner interface {
	// SignSessionToken creates a signed session JWT with standard claims
	// (iss, aud, typ=session, sub, email, roles). The host owns the signing
	// secret. The caller controls only the session identity.
	SignSessionToken(ctx context.Context, userID uuid.UUID, email string, roles []string) (string, error)
}
