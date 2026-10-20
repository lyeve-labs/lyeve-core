package security

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/google/uuid"
)

// MFAStore is the minimal interface the engine needs from an MFA plugin.
type MFAStore interface {
	IsEnabled(ctx context.Context, userID uuid.UUID) (bool, error)
	// GetEnabled returns the user's enabled authenticator-app secret and
	// recovery code hashes. A user without one, such as an account whose only
	// second factor is a passkey, gets an error wrapping core.ErrNotFound.
	GetEnabled(ctx context.Context, userID uuid.UUID) (encSecret string, backupHashes []string, err error)

	// ConsumeBackupCode removes codeHash from the user's unspent recovery
	// codes and reports whether it was one of them. It must read and remove
	// in one step, so of two requests carrying the same code only one sees
	// true. False means the code is not one of the user's unspent codes.
	ConsumeBackupCode(ctx context.Context, userID uuid.UUID, codeHash string) (bool, error)

	// HasWebAuthn decides whether the sign-in challenge offers a passkey as a
	// method. The Login handler lists WebAuthn in the challenge response only
	// when it answers true. It does not report whether the user holds a
	// credential: a store may answer false for a user who has one.
	HasWebAuthn(ctx context.Context, userID uuid.UUID) (bool, error)

	// DisableByAdmin removes MFA for a target user, recording the admin who
	// initiated the reset and a human-readable reason for the audit trail.
	// Returns an error if the target user has no MFA secrets.
	DisableByAdmin(ctx context.Context, targetUserID, adminUserID uuid.UUID, reason string) error

	// GracePeriodHours returns the enrollment grace period for new users.
	// During this window, MFA is optional. Returns 0 when grace period is disabled.
	GracePeriodHours() int

	// RegenerateBackupCodes replaces the current backup code set with n new
	// codes. Old codes are invalidated atomically. Returns the new plaintext
	// codes (shown once). Callers must hash before storage.
	RegenerateBackupCodes(ctx context.Context, userID uuid.UUID, n int) (newCodes []string, err error)

	// ConsumeTOTPStep records step as the user's last accepted TOTP time step
	// when it is later than the one recorded, and reports whether it was. It
	// must compare and record in one statement, so two requests carrying the
	// same code cannot both be accepted. False means the code was used before.
	ConsumeTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (bool, error)
}

// MFAStoreProvider is an optional interface plugins implement to expose
// their MFA store. The runtime detects it after Start() and passes the
// store to the admin router for AuthHandler consumption.
type MFAStoreProvider interface {
	core.Plugin
	MFAStore() MFAStore
}
