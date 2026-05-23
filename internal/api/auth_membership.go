package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// errNotAMember reports that the caller holds no membership in the tenant they
// asked to act in. It never reaches the client as its own status: answering
// differently from a wrong password would turn login into a probe for which
// tenants an address belongs to.
var errNotAMember = errors.New("no membership in the requested tenant")

// WithMemberships wires the cross-tenant membership reader a plugin supplied.
// Without it the auth path still works and every session acts in the account's
// home tenant, which is the single-tenant shape of this engine.
func (h *AuthHandler) WithMemberships(m core.MembershipReader) { h.memberships = m }

// resolveActingTenant decides which tenant a new session acts in, and with
// which roles.
//
// One account can hold several tenants, so issuing a token means choosing
// one. The rules, in order:
//
//   - No tenant requested: the account's home tenant, sys_users.tenant_id. A
//     membership for that tenant, if there is one, supplies the roles.
//     Otherwise the roles on the user row do. The home tenant never requires a
//     membership, because identity-provider plugins provision accounts by
//     creating the user row, with no membership.
//   - A tenant requested that is the home tenant: same as above.
//   - Any other tenant: a membership is required, and its roles are the roles
//     for the session. Roles are per membership on purpose, because an editor
//     at one brand and an admin at another is the case this exists for, and
//     carrying the home roles across would silently promote them.
//
// No reader at all is the single-tenant install. Every session then acts at
// home with the roles on its own row, and a request to act anywhere else is
// refused. Nothing is granted that the account row does not already grant.
//
// super_admin is the one exception. It already crosses tenants by header
// through the tenancy middleware, so requiring a membership row here would
// close a door that is open one layer down, and only for the login path.
func (h *AuthHandler) resolveActingTenant(ctx context.Context, user *domain.User, requested string) (string, []string, error) {
	home := user.TenantID

	if requested == "" || requested == home {
		roles := user.Roles
		if h.memberships != nil {
			m, err := h.memberships.Get(ctx, user.ID, home)
			switch {
			case err == nil:
				roles = m.Roles
			case errors.Is(err, core.ErrNotFound):
				// Home access does not depend on a membership existing.
			default:
				return "", nil, err
			}
		}
		return home, roles, nil
	}

	// A spelling the engine never issues is no tenant. MySQL and MSSQL match
	// the stored tenant case insensitively, so without this an upper-case
	// slug would resolve the membership of its lower-case twin and be signed
	// into the token as sent.
	if !core.IsValidTenantSlug(requested) {
		return "", nil, errNotAMember
	}

	if hasRole(user.Roles, "super_admin") {
		return requested, user.Roles, nil
	}

	if h.memberships == nil {
		return "", nil, errNotAMember
	}

	m, err := h.memberships.Get(ctx, user.ID, requested)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return "", nil, errNotAMember
		}
		return "", nil, err
	}
	return m.TenantID, m.Roles, nil
}

// hasRole reports whether roles contains name.
func hasRole(roles []string, name string) bool {
	for _, r := range roles {
		if r == name {
			return true
		}
	}
	return false
}

// respondTenantResolution maps a resolveActingTenant failure onto a response.
// A caller who is not a member is told exactly what a wrong password is told.
// A database failure is an outage and says so.
func respondTenantResolution(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNotAMember) {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}
	slog.Warn("membership lookup failed", "err", err)
	respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
}

// Memberships lists every tenant the caller may act in, so a client can offer
// a tenant switcher without guessing. The home tenant is always included, even
// when no membership row names it.
//
// GET /api/admin/auth/memberships
// Auth: any authenticated user. The answer is about the caller alone.
func (h *AuthHandler) Memberships(w http.ResponseWriter, r *http.Request) {
	claims := core.GetClaims(r.Context())
	if claims == nil {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}
	userID, err := uuid.Parse(claims.UserID)
	if err != nil {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}
	user, err := h.users.GetByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}

	type entry struct {
		TenantID string   `json:"tenant_id"`
		Roles    []string `json:"roles"`
		Home     bool     `json:"home"`
	}
	out := []entry{{TenantID: user.TenantID, Roles: user.Roles, Home: true}}

	if h.memberships != nil {
		list, err := h.memberships.ListForUser(r.Context(), userID)
		if err != nil {
			respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
			return
		}
		for _, m := range list {
			if m.TenantID == user.TenantID {
				out[0].Roles = m.Roles
				continue
			}
			out = append(out, entry{TenantID: m.TenantID, Roles: m.Roles})
		}
	}

	respond(w, http.StatusOK, map[string]any{"memberships": out})
}
