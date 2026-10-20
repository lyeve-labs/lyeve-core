package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The handler completes a challenge for a factor a plugin verified, such as a
// passkey, by the rules MFAVerify applies to a code. Both read and spend one
// record of used challenges, so a challenge gives one session whichever
// factor completes it.
var _ core.MFAChallengeCompleter = (*AuthHandler)(nil)

// MFAChallengeAccount implements core.MFAChallengeCompleter.
func (h *AuthHandler) MFAChallengeAccount(ctx context.Context, challengeToken string) (core.MFAChallengeAccount, error) {
	claims, userID, err := h.openChallenge(ctx, challengeToken)
	if err != nil {
		return core.MFAChallengeAccount{}, err
	}
	return core.MFAChallengeAccount{UserID: userID, Email: claims.Email, TenantID: claims.TenantID}, nil
}

// CompleteMFAChallenge implements core.MFAChallengeCompleter. The challenge
// is spent before anything is signed, as MFAVerify spends it.
func (h *AuthHandler) CompleteMFAChallenge(ctx context.Context, challengeToken string) (string, error) {
	claims, userID, err := h.openChallenge(ctx, challengeToken)
	if err != nil {
		return "", err
	}
	won, err := h.mfaLockoutChecker.MarkJTIUsed(ctx, claims.ID)
	if err != nil {
		return "", fmt.Errorf("spend mfa challenge: %w", err)
	}
	if !won {
		return "", core.ErrMFAChallengeInvalid
	}
	if h.users == nil {
		return "", errors.New("complete mfa challenge: no user store")
	}
	user, err := h.users.GetByID(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("complete mfa challenge: read account: %w", err)
	}
	if user.Disabled || (user.ExpiresAt != nil && time.Now().After(*user.ExpiresAt)) {
		return "", core.ErrAccountInactive
	}
	// The tenant is resolved again rather than read from the claim, so a
	// membership revoked since the password step is caught here.
	tenantID, roles, err := h.resolveActingTenant(ctx, user, claims.TenantID)
	if errors.Is(err, errNotAMember) {
		return "", core.ErrMFAChallengeInvalid
	}
	if err != nil {
		return "", fmt.Errorf("complete mfa challenge: resolve tenant: %w", err)
	}
	token, err := auth.Sign(h.jwtSecret, h.expirySecs, userID, user.SignInEmail(), roles, tenantID, user.TokenVersion)
	if err != nil {
		return "", fmt.Errorf("complete mfa challenge: sign: %w", err)
	}
	return token, nil
}

// openChallenge checks a challenge token as MFAVerify does before it reads a
// factor: a valid pending challenge, issued on a host where it may complete,
// and not yet spent. A challenge with no id cannot be spent, so it is
// refused here, where MFAVerify would let it through.
func (h *AuthHandler) openChallenge(ctx context.Context, challengeToken string) (*auth.Claims, uuid.UUID, error) {
	claims, err := auth.ParseMulti([]string{h.jwtSecret}, challengeToken)
	if err != nil || !claims.MFAPending || claims.ID == "" {
		return nil, uuid.Nil, core.ErrMFAChallengeInvalid
	}
	if host := signInHostTenant(ctx); host != "" && claims.TenantID != host {
		return nil, uuid.Nil, core.ErrMFAChallengeInvalid
	}
	userID, err := uuid.Parse(claims.UserID)
	if err != nil {
		return nil, uuid.Nil, core.ErrMFAChallengeInvalid
	}
	used, err := h.mfaLockoutChecker.IsJTIUsed(ctx, claims.ID)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("read mfa challenge state: %w", err)
	}
	if used {
		return nil, uuid.Nil, core.ErrMFAChallengeInvalid
	}
	return claims, userID, nil
}
