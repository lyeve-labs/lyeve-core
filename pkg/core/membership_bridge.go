package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Membership is one account's right to act inside one tenant.
//
// The account itself is global: an address is one account however many brands
// the person works on. This type is the per-tenant half, which tenants that
// account may act in and with which roles in each. An editor at one brand and
// an admin at another is two memberships and still one account.
type Membership struct {
	UserID    uuid.UUID `json:"user_id"`
	TenantID  string    `json:"tenant_id"`
	Roles     []string  `json:"roles"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MembershipReader answers which tenants an account may act in beyond its own.
// It is the read half of cross-tenant membership, which is all the engine's
// sign-in path needs: choosing the tenant a new session acts in, and listing
// the tenants a client may offer as a switcher.
//
// An account's home tenant is not in here and never needs to be. Home access
// comes from the account row, so a membership is an additional tenant or an
// override of the roles the account holds at home. A reader that answers
// nothing at all is a correct single-tenant install, not a broken one.
//
// A tenant the account holds no membership in is ErrNotFound. Every other
// error is an outage and the caller answers 503.
type MembershipReader interface {
	// Get returns the account's membership in one tenant, or ErrNotFound.
	Get(ctx context.Context, userID uuid.UUID, tenantID string) (*Membership, error)

	// ListForUser returns every membership the account holds, ordered by
	// tenant so the answer is stable across calls.
	ListForUser(ctx context.Context, userID uuid.UUID) ([]*Membership, error)
}

// MembershipProvider is an optional interface a plugin implements to tell the
// engine which accounts may act in which tenants. The runtime reads it from
// the one running plugin that implements it, and hands the reader to both
// routers.
//
// Without it every session acts in the account's home tenant, and a sign-in
// asking to act anywhere else is refused with exactly what a wrong password is
// refused with. That is the safe default in both directions: nothing is
// granted that the account row does not already grant, and nobody is locked
// out of the tenant they belong to. It is also the ordinary shape of a
// single-tenant install rather than a degraded one.
//
// Returning nil from MembershipReader is the same as not implementing this at
// all, so a plugin whose tenancy is off may say so without the engine having
// to know why.
type MembershipProvider interface {
	Plugin

	// MembershipReader returns the reader, or nil to supply none.
	MembershipReader() MembershipReader
}
