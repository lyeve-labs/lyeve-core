package core

import (
	"context"

	"github.com/google/uuid"
)

// PasskeyRevoker removes every passkey an account holds, with the passkey
// ceremonies it has in flight, so none of them signs in again. It answers the
// number of passkeys removed, 0 for an account with none. Removing the last
// factor turns MFA off for the account, as removing one passkey does.
//
// A passkey is a sign-in on its own, so one added from a stolen session
// outlasts a new password. A password reset asks this before it changes the
// password. The engine host implements it by asking the running plugin that
// supplies the role, and ScopedHost forwards it to a plugin granted
// CapPasskeyRevoke and answers ErrCapDenied to every other.
type PasskeyRevoker interface {
	RevokePasskeys(ctx context.Context, userID uuid.UUID) (int, error)
}

// PasskeyRevokerProvider is implemented by the plugin that stores passkeys.
// The engine finds it by this interface, and one running plugin at most may
// hold it.
type PasskeyRevokerProvider interface {
	PasskeyRevoker() PasskeyRevoker
}
