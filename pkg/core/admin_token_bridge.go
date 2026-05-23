package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AdminToken is a machine's credential for the admin API: owned by a user,
// bound to one tenant, limited to its grants, and always expiring. The token
// itself is never stored. TokenHash is its peppered hash and DisplayPrefix the
// first characters after the prefix, for telling tokens apart.
type AdminToken struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      string     `json:"tenant_id"`
	OwnerUserID   uuid.UUID  `json:"owner_user_id"`
	OwnerEmail    string     `json:"owner_email"`
	OwnerErased   bool       `json:"-"`
	Name          string     `json:"name"`
	TokenHash     string     `json:"-"`
	DisplayPrefix string     `json:"display_prefix"`
	Grants        []string   `json:"grants"`
	AllowedIPs    []string   `json:"allowed_ips"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CreatedAt     time.Time  `json:"created_at"`
	LastUsedAt    *time.Time `json:"last_used_at"`
	RevokedAt     *time.Time `json:"revoked_at"`
	RotatedFrom   *uuid.UUID `json:"rotated_from"`
	// ReplacedBy is the newest token rotated from this one, filled by List.
	ReplacedBy *uuid.UUID `json:"replaced_by"`
}

// HasGrant reports whether the token holds grant.
func (t *AdminToken) HasGrant(grant string) bool {
	for _, g := range t.Grants {
		if g == grant {
			return true
		}
	}
	return false
}

// AdminTokenRequest is one request an admin token made on the admin API,
// keyed by the route pattern rather than the raw path.
type AdminTokenRequest struct {
	ID           uuid.UUID `json:"id"`
	TokenID      uuid.UUID `json:"token_id"`
	TenantID     string    `json:"tenant_id"`
	Method       string    `json:"method"`
	RoutePattern string    `json:"route_pattern"`
	Status       int       `json:"status"`
	ClientIP     string    `json:"client_ip"`
	CreatedAt    time.Time `json:"created_at"`
}

// AdminTokenStore keeps admin tokens and the record of what each one did. It
// is storage and nothing else: the engine decides which token is honored, on
// which routes, from which addresses and for how long, and it hashes the
// secret before this interface ever sees it.
//
// An implementation stores what it is given and reads it back. It never
// interprets a grant, an expiry or an address list, because the engine has
// already checked them and a second opinion here would be a second policy.
//
// A token that is not stored is ErrNotFound. Every other error is an outage,
// and the engine refuses the request rather than admitting it.
type AdminTokenStore interface {
	// Create stores a new token. The caller has already filled TokenHash.
	Create(ctx context.Context, t *AdminToken) error

	// GetByHash returns the token with that peppered hash, or ErrNotFound.
	// It is the authentication read, so it is not scoped by tenant: the
	// token names the tenant it acts in and the engine checks that after.
	GetByHash(ctx context.Context, hash string) (*AdminToken, error)

	// Get returns one of the tenant's tokens by id, or ErrNotFound.
	Get(ctx context.Context, tenantID string, id uuid.UUID) (*AdminToken, error)

	// List returns a page of the tenant's tokens and the total, optionally
	// narrowed to one owner. ReplacedBy is filled on the page.
	List(ctx context.Context, tenantID string, owner *uuid.UUID, limit, offset int) ([]*AdminToken, int, error)

	// Revoke stamps the token revoked at the given time, or ErrNotFound.
	Revoke(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error

	// Rotate stores the successor and lets the old token keep working until
	// overlapUntil, so a caller swapping credentials is never locked out
	// mid-deploy. Both halves happen together or neither does.
	Rotate(ctx context.Context, tenantID string, oldID uuid.UUID, successor *AdminToken, overlapUntil time.Time) error

	// TouchLastUsed records that the token was used. It runs off the request
	// path, so a failure is logged and never refuses a caller.
	TouchLastUsed(ctx context.Context, tenantID string, id uuid.UUID, at time.Time) error

	// LogRequests appends a batch of request records.
	LogRequests(ctx context.Context, rows []AdminTokenRequest) error

	// ListRequests returns a page of one token's requests and the total.
	ListRequests(ctx context.Context, tenantID string, tokenID uuid.UUID, limit, offset int) ([]*AdminTokenRequest, int, error)

	// PruneRequests deletes request records older than before, at most limit
	// per call, and returns how many went. The engine calls it on a timer.
	PruneRequests(ctx context.Context, before time.Time, limit int) (int64, error)
}

// AdminTokenStoreProvider is an optional interface a plugin implements to keep
// the engine's admin tokens. The runtime reads it from the one running plugin
// that implements it, and hands the store to the admin router.
//
// Without it the engine has nowhere to read a token from, so it refuses every
// one presented and serves none of the routes that issue them. That is the
// only safe reading: an admin token is a credential, and a credential store
// that cannot answer has to be treated as answering no. Session sign-in is
// untouched, so an install with no store is administered by people rather than
// by machines.
//
// Returning nil from AdminTokenStore is the same as not implementing this at
// all.
type AdminTokenStoreProvider interface {
	Plugin

	// AdminTokenStore returns the store, or nil to supply none.
	AdminTokenStore() AdminTokenStore
}
