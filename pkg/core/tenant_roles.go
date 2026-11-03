package core

import (
	"context"

	"github.com/google/uuid"
)

// TenantRolesReader answers which roles an account holds in one tenant,
// resolved the way a sign-in into that tenant resolves them: in the
// account's home tenant its membership there or else the roles on its own
// row, in any other tenant its membership there, and super_admin from the
// account's row alone. A plugin that acts for a person long after their
// session ended reads it to learn whether the person still holds a role,
// since a membership granted, lowered or revoked moves no token version.
//
// The engine host implements it, and ScopedHost forwards it to a plugin
// granted CapTenantRoles and answers ErrCapDenied to every other.
type TenantRolesReader interface {
	// TenantRoles answers the account's roles in tenantID. A tenant the
	// account may not act in, and an account that does not exist, are
	// ErrNotFound. Any other error is an outage the caller may retry.
	TenantRoles(ctx context.Context, userID uuid.UUID, tenantID string) ([]string, error)
}
