package security

import (
	"context"

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/google/uuid"
)

// MFAStore is the minimal interface the engine needs from an MFA plugin.
type MFAStore interface {
	IsEnabled(ctx context.Context, userID uuid.UUID) (bool, error)
	GetEnabled(ctx context.Context, userID uuid.UUID) (encSecret string, backupHashes []string, err error)
	UpdateBackupCodes(ctx context.Context, userID uuid.UUID, hashes []string) error

	// HasWebAuthn reports whether the user has at least one WebAuthn/passkey
	// credential registered. The Login handler uses this to advertise WebAuthn
	// as an available MFA method in the challenge response.
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
