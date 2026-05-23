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
	return &out
}
