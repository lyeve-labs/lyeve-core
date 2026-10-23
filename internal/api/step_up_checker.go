package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin asks the handler the token routes ask, so its checks count
// against the same lockouts and refuse a replayed code the same way.
var _ core.StepUpChecker = (*AuthHandler)(nil)

// CheckStepUp implements core.StepUpChecker for the account the request's
// session names. Whether the account can give the factor is answered first
// and an empty answer second, so neither reaches a lockout. The answer is
// then checked by stepUpPassword or stepUpCode, whose counting is the token
// routes' own.
func (h *AuthHandler) CheckStepUp(r *http.Request, factor, answer string) error {
	ctx := r.Context()
	u, err := h.stepUpAccount(r)
	if err != nil {
		return err
	}
	var status int
	switch factor {
	case core.StepUpPassword:
		if !hasPassword(u.PasswordHash) {
			return core.ErrStepUpUnavailable
		}
		if answer == "" {
			return core.ErrStepUpRequired
		}
		status, _ = h.stepUpPassword(r, u, answer)
	case core.StepUpCode:
		if h.mfaStore == nil {
			return core.ErrStepUpUnavailable
		}
		enrolled, err := h.mfaStore.IsEnabled(ctx, u.ID)
		if err != nil {
			slog.WarnContext(ctx, "step-up mfa check failed", "err", err)
			return fmt.Errorf("step-up mfa check: %w", core.ErrServiceUnavailable)
		}
		if !enrolled {
			return core.ErrStepUpUnavailable
		}
		encSecret, _, err := h.mfaStore.GetEnabled(ctx, u.ID)
		if errors.Is(err, core.ErrNotFound) {
			// The only second factor is a passkey, which holds no code.
			return core.ErrStepUpUnavailable
		}
		if err != nil {
			slog.WarnContext(ctx, "step-up totp secret read failed", "err", err, "user_id", u.ID)
			return fmt.Errorf("step-up totp secret read: %w", core.ErrServiceUnavailable)
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return core.ErrStepUpRequired
		}
		status, _ = h.stepUpCode(r, u, encSecret, answer)
	default:
		return fmt.Errorf("step-up factor %q is not one the engine checks: %w", factor, core.ErrServiceUnavailable)
	}
	switch status {
	case 0:
		return nil
	case http.StatusForbidden:
		return core.ErrStepUpRefused
	case http.StatusTooManyRequests:
		return core.ErrStepUpLocked
	default:
		return fmt.Errorf("step-up %s check: %w", factor, core.ErrServiceUnavailable)
	}
}

// stepUpAccount reads the account a signed-in session names. A request that
// carries no session, or carries an API key or an admin token, has no person
// at it to ask.
//
// The account has to belong to the tenant the request resolved to, because a
// plugin can set the session claims on its own request and would otherwise
// spend the lockouts of an account in any tenant. An account with no home
// tenant acts in the default tenant, and a super admin with none acts in
// every tenant.
func (h *AuthHandler) stepUpAccount(r *http.Request) (*domain.User, error) {
	claims := core.GetClaims(r.Context())
	if claims == nil || claims.IsAPIKey || claims.APIKeyID != "" || claims.AdminTokenID != "" {
		return nil, core.ErrStepUpUnavailable
	}
	uid, err := uuid.Parse(claims.UserID)
	if err != nil {
		return nil, core.ErrStepUpUnavailable
	}
	u, err := h.users.GetByID(r.Context(), uid)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, core.ErrStepUpUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("step-up account: %w", errors.Join(err, core.ErrServiceUnavailable))
	}
	tenant := core.TenantIDFromCtx(r.Context())
	switch {
	case u.TenantID == tenant:
	case u.TenantID == "" && (tenant == core.DefaultTenantSlug || hasRole(u.Roles, "super_admin")):
	default:
		return nil, core.ErrStepUpUnavailable
	}
	return u, nil
}

// hasPassword reports whether hash is a password the account can give, as
// opposed to no password or a marker for an account that signs in elsewhere.
func hasPassword(hash string) bool {
	switch hash {
	case "", auth.OAuthOnlyMarker, auth.ExternalIssuerMarker:
		return false
	}
	return true
}
