package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin that acts for a person reads their roles the way a sign-in
// resolves them, so the two never disagree about who may act where.
var _ core.TenantRolesReader = (*AuthHandler)(nil)

// TenantRoles implements core.TenantRolesReader with the rules of
// resolveActingTenant. An account with no home tenant on its row belongs to
// the default tenant, as the step-up check reads it. It answers roles only:
// whether the account may still sign in at all, disabled, erased or past its
// expiry, is the caller's to check.
func (h *AuthHandler) TenantRoles(ctx context.Context, userID uuid.UUID, tenantID string) ([]string, error) {
	// resolveActingTenant reads an empty tenant as the account's home, which
	// a caller that named no tenant never means.
	if tenantID == "" {
		return nil, core.ErrNotFound
	}
	u, err := h.users.GetByID(ctx, userID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant roles: %w", errors.Join(err, core.ErrServiceUnavailable))
	}
	acct := u.Clone()
	if acct.TenantID == "" {
		acct.TenantID = core.DefaultTenantSlug
	}
	_, roles, err := h.resolveActingTenant(ctx, acct, tenantID)
	if errors.Is(err, errNotAMember) {
		return nil, core.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant roles: %w", errors.Join(err, core.ErrServiceUnavailable))
	}
	return roles, nil
}
