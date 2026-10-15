package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// NewTenantAccountStore serves core.TenantAccountStore from the user store
// over pool, translating its rows and errors into the plugin-facing types.
func NewTenantAccountStore(pool DB) core.TenantAccountStore {
	return tenantAccounts{users: NewUserStore(pool)}
}

type tenantAccounts struct {
	users *UserStore
}

func (t tenantAccounts) FindTenantAccount(ctx context.Context, tenantID, address string) (*core.TenantAccount, error) {
	u, err := t.users.GetByTenantAddress(ctx, tenantID, address)
	if err != nil {
		return nil, coreUserErr("find tenant account", err)
	}
	return tenantAccountOf(u), nil
}

func (t tenantAccounts) FindProviderAccount(ctx context.Context, tenantID, address string) (*core.TenantAccount, error) {
	u, err := t.users.GetByTenantProviderEmail(ctx, tenantID, address)
	if err != nil {
		return nil, coreUserErr("find provider account", err)
	}
	return tenantAccountOf(u), nil
}

func (t tenantAccounts) CreateTenantAccount(ctx context.Context, in core.NewTenantAccount) (*core.TenantAccount, bool, error) {
	u, created, err := t.users.CreateTenantLocal(ctx, in.TenantID, in.Address, in.PasswordHash, in.Roles, in.ProviderSeen)
	if err != nil {
		return nil, false, coreUserErr("create tenant account", err)
	}
	return tenantAccountOf(u), created, nil
}

func (t tenantAccounts) StampProviderSeen(ctx context.Context, tenantID string, id uuid.UUID) error {
	if err := t.users.StampProviderSeen(ctx, tenantID, id); err != nil {
		return coreUserErr("stamp provider seen", err)
	}
	return nil
}

func tenantAccountOf(u *domain.User) *core.TenantAccount {
	return &core.TenantAccount{
		ID:             u.ID,
		TenantID:       u.TenantID,
		Email:          u.Email,
		ProviderEmail:  u.ProviderEmail,
		ProviderSeenAt: u.ProviderSeenAt,
		Roles:          append([]string(nil), u.Roles...),
		TokenVersion:   u.TokenVersion,
		Disabled:       u.Disabled,
		ExpiresAt:      u.ExpiresAt,
		TrustedIssuer:  u.PasswordHash == auth.ExternalIssuerMarker,
	}
}

// coreUserErr maps the user store's errors onto the sentinels a plugin can
// test with errors.Is.
func coreUserErr(op string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%s: %w", op, core.ErrNotFound)
	case errors.Is(err, domain.ErrConflict):
		return fmt.Errorf("%s: %w: %w", op, core.ErrConflict, err)
	case errors.Is(err, domain.ErrValidation):
		return fmt.Errorf("%s: %w: %w", op, core.ErrValidation, err)
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}
