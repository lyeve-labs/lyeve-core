package core

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Tenant-local accounts.
//
// An account a tenant's identity provider or sign-up form creates belongs to
// that tenant alone. sys_users.email is unique across the install, so such an
// account stores a key there that no other row can hold, and keeps the address
// its provider asserted in sys_users.provider_email. It is found by its tenant
// plus that address, never by the install-wide email, so a sign-up answers the
// same whether another tenant holds the address or nobody does.
//
// sys_users.provider_seen_at is when the tenant's provider last signed the
// account in. A provider that stops asserting someone sends nothing, so password
// login, password reset and magic link refuse an account whose stamp is older
// than its tenant's window. NULL means no provider vouches for the account,
// and the window does not apply to it.

// tenantLocalEmailSuffix is the domain of every tenant-local key. The .invalid
// top-level domain is reserved, so no mailbox can ever be reached at a key.
const tenantLocalEmailSuffix = "@external.invalid"

// TenantLocalEmail is the value a tenant-local account stores in
// sys_users.email: its own id at a reserved domain. Every door that creates
// such an account writes this value, so the doors cannot drift, and the id
// makes it unique across the install.
func TenantLocalEmail(id uuid.UUID) string {
	return id.String() + tenantLocalEmailSuffix
}

// IsTenantLocalEmail reports whether email is the tenant-local key of the
// account id.
func IsTenantLocalEmail(id uuid.UUID, email string) bool {
	return email == TenantLocalEmail(id)
}

// The provider window: how long after its provider last signed an account in
// that account may still sign in by password, password reset or magic link.
const (
	// ProviderWindowSettingKey is the key in the tenant's engine settings row
	// (sys_plugin_config, plugin_name "core") that holds the window in days.
	ProviderWindowSettingKey = "auth.provider_window_days"
	// ProviderWindowDefaultDays is the window of a tenant that never set one.
	ProviderWindowDefaultDays = 30
	// ProviderWindowMaxDays is the longest window a tenant may set, so a
	// forgotten setting cannot turn the window off.
	ProviderWindowMaxDays = 365
)

// ProviderWindowAdmits reports whether an account its provider last signed in
// at seenAt is inside a window of days at now. A nil seenAt is an account no
// provider vouches for, which the window does not cover. A window of 0 admits
// no provider account: it signs in only through its provider.
func ProviderWindowAdmits(seenAt *time.Time, days int, now time.Time) bool {
	if seenAt == nil {
		return true
	}
	return now.Sub(*seenAt) < time.Duration(days)*24*time.Hour
}

// ProviderWindowProvider is implemented by the engine host. It is the one
// reader of a tenant's window: password login, password reset and magic link
// all call it, so the three cannot disagree. Nothing is cached, so a change
// applies to the next call.
type ProviderWindowProvider interface {
	// ProviderWindowDays returns tenantID's window in days, or
	// ProviderWindowDefaultDays when the tenant never set one.
	ProviderWindowDays(ctx context.Context, tenantID string) (int, error)
	// ProviderWindowAdmitsAccount reports whether an account in tenantID that
	// its provider last signed in at seenAt is inside the tenant's window now.
	// A nil seenAt is admitted without reading the setting.
	ProviderWindowAdmitsAccount(ctx context.Context, tenantID string, seenAt *time.Time) (bool, error)
}

// TenantAccount is a sys_users row as a tenant-scoped door reads it. Email is
// the stored column: the address for an ordinary account, the key for a
// tenant-local one. ProviderEmail is "" when the column is NULL.
type TenantAccount struct {
	ID             uuid.UUID
	TenantID       string
	Email          string
	ProviderEmail  string
	ProviderSeenAt *time.Time
	Roles          []string
	TokenVersion   int
	Disabled       bool
	ExpiresAt      *time.Time
	// TrustedIssuer marks an account a trusted token issuer created. It never
	// signs in by password, reset or magic link.
	TrustedIssuer bool
}

// SignInEmail is the address a session for the account carries: the address
// its provider asserted when there is one, the stored email otherwise.
func (a *TenantAccount) SignInEmail() string {
	if a.ProviderEmail != "" {
		return a.ProviderEmail
	}
	return a.Email
}

// NewTenantAccount is what a door supplies to create a tenant-local account.
type NewTenantAccount struct {
	TenantID string
	// Address is the address the provider asserted or the link was mailed
	// to. It is normalized with NormalizeEmail before it is stored.
	Address string
	// PasswordHash is stored as given: a door's own no-password marker, or
	// the empty string.
	PasswordHash string
	// Roles defaults to editor when empty.
	Roles []string
	// ProviderSeen stamps provider_seen_at with the creation time. A door
	// whose provider vouches for the person (SAML, OAuth) sets it. Magic
	// link leaves it false: the mailbox vouches, not a provider.
	ProviderSeen bool
}

// TenantAccountStore finds, creates and stamps accounts by tenant and address.
// Every method reads and writes only the named tenant's rows, so a free
// address and one another tenant holds run the same statements.
//
// Errors: ErrNotFound when no row matches, ErrConflict when a create cannot
// complete because of another row, and a wrapped database error otherwise.
type TenantAccountStore interface {
	// FindTenantAccount returns the account in tenantID whose email or
	// provider_email is address. This is the read an SSO door makes before
	// it creates: it finds the tenant's own password account with that
	// address as well as a tenant-local one. When both exist, the
	// tenant-local account wins.
	FindTenantAccount(ctx context.Context, tenantID, address string) (*TenantAccount, error)
	// FindProviderAccount returns the account in tenantID whose
	// provider_email is address. This is the read password reset makes
	// before it falls back to the install-wide email.
	FindProviderAccount(ctx context.Context, tenantID, address string) (*TenantAccount, error)
	// CreateTenantAccount inserts a tenant-local account keyed by
	// TenantLocalEmail. When a concurrent first sign-in inserted the same
	// tenant and address first, the insert fails on the unique index and the
	// winner is returned with created false. Any other failure is an error.
	CreateTenantAccount(ctx context.Context, in NewTenantAccount) (acct *TenantAccount, created bool, err error)
	// StampProviderSeen sets provider_seen_at to now on the account id in
	// tenantID. A door calls it at every successful sign-in through its
	// provider. ErrNotFound when the tenant holds no such account.
	StampProviderSeen(ctx context.Context, tenantID string, id uuid.UUID) error
}

// TenantAccountProvider is implemented by the engine host.
type TenantAccountProvider interface {
	TenantAccounts() TenantAccountStore
}
