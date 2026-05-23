package domain

import (
	"time"

	"github.com/google/uuid"
)

// Device sign-in states. A request starts pending, a signed-in admin approves
// or denies it once, and an approved request is used by the one exchange that
// turns it into a session.
const (
	DeviceLoginPending  = "pending"
	DeviceLoginApproved = "approved"
	DeviceLoginDenied   = "denied"
	DeviceLoginUsed     = "used"
)

// DeviceLogin is a sign-in a device started and a person approves in a
// browser. The device code is never stored. DeviceCodeHash is its SHA-256.
// UserID, TenantID and TokenVersion are set when the request is approved:
// the approver, the tenant their session acts in and the token version that
// session was signed under.
type DeviceLogin struct {
	ID             uuid.UUID
	DeviceCodeHash string
	UserCode       string
	ClientName     string
	RequesterIP    string
	// RequesterNet is what the open-request cap counts by: the address for
	// IPv4, its /64 for IPv6.
	RequesterNet string
	Status       string
	UserID       *uuid.UUID
	TenantID     *string
	TokenVersion *int
	PollInterval int
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ApprovedAt   *time.Time
	LastPolledAt *time.Time
}
