package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// passwordAccount finds the account a password login names by address.
//
// An account a tenant's provider created belongs to that tenant alone and
// keeps the address in provider_email, so it is reachable only from a request
// that names its tenant. The tenant the request resolved by itself (a mapped
// hostname, the sole tenant, a single-tenant install) wins. The body's tenant
// field names it only where the request resolved none, so a sign-in on one
// tenant's hostname never reaches another tenant's account by naming it.
// When a tenant is known that read runs first, whatever it finds, and the
// install-wide email is the fallback. So an address another tenant's provider
// account holds is answered as a free one: neither read can reach that row.
//
// A body tenant the engine never issues names no tenant. Where the host names
// no tenant, the field still chooses which of an account's own memberships the
// session acts in, which resolveSignInTenant decides and limits to those
// memberships.
//
// On a tenant's mapped domain an account the domain's tenant does not admit is
// not found, so it takes the unknown-address path before its password or its
// lockout is read (see hostAdmitsAccount).
func (h *AuthHandler) passwordAccount(ctx context.Context, email, bodyTenant string) (*domain.User, error) {
	u, err := h.findPasswordAccount(ctx, email, bodyTenant)
	if err != nil {
		return nil, err
	}
	admitted, err := h.hostAdmitsAccount(ctx, u)
	if err != nil {
		return nil, err
	}
	if !admitted {
		return nil, domain.ErrNotFound
	}
	return u, nil
}

func (h *AuthHandler) findPasswordAccount(ctx context.Context, email, bodyTenant string) (*domain.User, error) {
	tenant := core.TenantIDFromCtx(ctx)
	if tenant == "" && core.IsValidTenantSlug(bodyTenant) {
		tenant = bodyTenant
	}
	if tenant != "" {
		u, err := h.users.GetByTenantProviderEmail(ctx, tenant, email)
		if err == nil {
			return u, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
	}
	return h.users.GetByEmail(ctx, email)
}

// hostAdmitsAccount reports whether a sign-in on the request's host may go on
// for user. A host that names no tenant, or one mapped to the default tenant,
// admits every account. Any other mapped domain admits its own accounts,
// members of its tenant and super admins. Any other account is answered as an
// unknown address, so a failed attempt there counts nothing, locks nothing and
// records nothing, and the domain's tenant cannot tell an address another
// tenant holds from a free one.
func (h *AuthHandler) hostAdmitsAccount(ctx context.Context, user *domain.User) (bool, error) {
	host := signInHostTenant(ctx)
	if host == "" || user.TenantID == host || hasRole(user.Roles, "super_admin") {
		return true, nil
	}
	if h.memberships == nil {
		return false, nil
	}
	_, err := h.memberships.Get(ctx, user.ID, host)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, core.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// failedAttemptContext is the context a failed password sign-in logs and
// records under. A tenant reads its own log stream, and the fallback lookup
// above reaches an account of any tenant, so a line naming that account under
// the request's tenant tells the domain's tenant the address holds an account
// somewhere on the install. An address that holds none writes nothing. Where
// the account's tenant differs from the one the request resolved, the attempt
// is stamped with the account's tenant, which is also where its own admins
// look. A request that resolved no tenant keeps its context.
func failedAttemptContext(ctx context.Context, user *domain.User) context.Context {
	if tenant := core.TenantIDFromCtx(ctx); tenant != "" && tenant != user.TenantID {
		return core.WithTenantID(ctx, user.TenantID)
	}
	return ctx
}

// respondIfPasswordSignInRefused answers a password sign-in for an account
// that may not sign in by password, and reports whether the sign-in may go
// on. A disabled or expired account, and a provider account outside its
// tenant's window, are each answered exactly as a wrong password is. The
// caller has already run the password compare, so the timing matches too.
func (h *AuthHandler) respondIfPasswordSignInRefused(w http.ResponseWriter, r *http.Request, user *domain.User) bool {
	if user.Disabled || (user.ExpiresAt != nil && time.Now().After(*user.ExpiresAt)) {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return false
	}
	admitted, err := h.providerWindowAdmits(r.Context(), user)
	if err != nil {
		slog.WarnContext(r.Context(), "provider window read failed", "err", err, "user_id", user.ID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return false
	}
	if !admitted {
		slog.InfoContext(r.Context(), "password sign-in refused: the provider has not signed the account in within its tenant's window",
			"user_id", user.ID, "tenant", user.TenantID)
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return false
	}
	return true
}

// providerWindowAdmits reports whether user is inside its tenant's provider
// window. An account no provider vouches for always is.
func (h *AuthHandler) providerWindowAdmits(ctx context.Context, user *domain.User) (bool, error) {
	if user.ProviderSeenAt == nil {
		return true, nil
	}
	if h.providerWindows == nil {
		return core.ProviderWindowAdmits(user.ProviderSeenAt, core.ProviderWindowDefaultDays, time.Now()), nil
	}
	return h.providerWindows.ProviderWindowAdmitsAccount(ctx, user.TenantID, user.ProviderSeenAt)
}
