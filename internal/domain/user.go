package domain

import (
	"time"

	"github.com/google/uuid"
)

// User is a CMS admin user.
type User struct {
	ID           uuid.UUID  `json:"id"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	Roles        []string   `json:"roles"`
	TenantID     string     `json:"tenant_id"`
	TokenVersion int        `json:"token_version"`
	Disabled     bool       `json:"disabled"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	// ProviderEmail is the address a tenant's provider asserted for a
	// tenant-local account, whose Email holds a key instead. "" for every
	// other account.
	ProviderEmail string `json:"provider_email,omitempty"`
	// ProviderSeenAt is when the tenant's provider last signed the account
	// in. Nil when no provider vouches for it.
	ProviderSeenAt *time.Time `json:"provider_seen_at,omitempty"`
}

// SignInEmail is the address a session for u carries: the address its
// provider asserted when there is one, the stored email otherwise.
func (u *User) SignInEmail() string {
	if u.ProviderEmail != "" {
		return u.ProviderEmail
	}
	return u.Email
}

// Clone returns an independent deep copy of u.
func (u *User) Clone() *User {
	if u == nil {
		return nil
	}
	out := *u
	out.Roles = append([]string(nil), u.Roles...)
	if u.ExpiresAt != nil {
		v := *u.ExpiresAt
		out.ExpiresAt = &v
	}
	if u.ProviderSeenAt != nil {
		v := *u.ProviderSeenAt
		out.ProviderSeenAt = &v
	}
	return &out
}
