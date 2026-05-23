package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// Logout clears the session cookie and revokes the refresh token family.
// POST /api/admin/auth/logout
// Body: {"refresh_token": "..."} (optional, best-effort revocation)
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	// Extract family ID from the refresh_token body for revocation.
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = jsonpool.DecodeJSON(r.Body, &body) // best-effort. Revocation is fire-and-forget

	if body.RefreshToken != "" && h.refreshTokenStore != nil {
		familyID, _, err := parseTokenForFamily(body.RefreshToken)
		if err == nil {
			// Fire-and-forget: the cookie is cleared regardless.
			if err := h.refreshTokenStore.RevokeFamily(r.Context(), familyID); err != nil {
				slog.Warn("failed to revoke refresh token family on logout",
					"err", err, "family_id", familyID)
			}
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     security.SessionCookieNameFor(h.secureCook),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCook,
		SameSite: http.SameSiteStrictMode,
	})
	h.clearCSRFCookie(w)

	// Bump token_version to invalidate all outstanding access tokens.
	// Fire-and-forget w.r.t. the response: the cookies are cleared
	// regardless.
	if claims := claimsFromCtx(r); claims != nil {
		if uid, err := uuid.Parse(claims.UserID); err == nil {
			if err := h.users.BumpTokenVersion(r.Context(), uid); err != nil {
				slog.Warn("failed to bump token version on logout",
					"err", err, "user_id", claims.UserID)
			}
		}
	}

	respond(w, http.StatusOK, map[string]string{"message": "logged out"})
}

// parseTokenForFamily extracts the family ID from an opaque refresh token.
// This is a lightweight parse (no Redis access) used for logout revocation.
func parseTokenForFamily(token string) (familyID string, _ string, err error) {
	return auth.RefreshTokenFamilyID(token)
}

// Me returns the currently authenticated user.
// GET /api/admin/auth/me
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	claims := claimsFromCtx(r)
	if claims == nil {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
		return
	}
	respond(w, http.StatusOK, map[string]any{
		"id":    claims.UserID,
		"email": claims.Email,
		"roles": claims.Roles,
	})
}

// Token issues a Bearer token for external API clients.
// POST /api/v1/auth/token
// Body: {"email": "...", "password": "..."}
// Auth: public (rate-limited).
func (h *AuthHandler) Token(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		// Tenant names which of the caller's tenants this token acts in.
		// Empty means their home tenant.
		Tenant string `json:"tenant"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}

	if body.Email == "" || body.Password == "" {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	// Same reasoning as Login: a control character rules the address out before
	// the database sees it, and answering 503 there would turn malformed input into
	// a reported outage.
	if hasControlChars(body.Email) {
		_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	user, err := h.users.GetByEmail(r.Context(), body.Email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Enumeration prevention: unknown email -> dummy bcrypt compare.
			_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password)
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}

	// Account lockout check (before password verification)
	if h.lockoutChecker != nil {
		count, lerr := h.lockoutChecker.CountRecentFailures(r.Context(), user.ID, time.Now().Add(-h.LockoutWindow))
		if lerr != nil {
			slog.Warn("lockout check failed in Token, failing closed", "err", lerr, "user_id", user.ID)
			_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
		if count >= h.MaxFailedAttempts {
			// Answer as the admin path does: 401 with the ordinary invalid
			// credentials code, and a dummy compare so the timing matches. Only
			// an account that exists can be locked, so a status of its own, a
			// code of its own, or a fast answer all tell a caller the address is
			// real. TestToken_EnumerationTiming_Unit measures exactly that.
			slog.WarnContext(r.Context(), "token login refused: account is locked out",
				"user_id", user.ID,
				"failures", count,
				"max_attempts", h.MaxFailedAttempts,
				"window", h.LockoutWindow,
				"note", h.lockoutScopeNote())
			_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
	}

	if err := auth.VerifyPassword(h.passwordHashAlgo, user.PasswordHash, body.Password); err != nil {
		slog.InfoContext(r.Context(), "token login failed: password did not verify",
			"user_id", user.ID,
			"credential_supplied", body.Password != "",
			"hash_present", user.PasswordHash != "",
			"algo", h.passwordHashAlgo)
		// Count it against the lockout, which this path checks above. The
		// public rate limiter bounds throughput, not attempts, so this count is
		// what limits guessing on this endpoint.
		if h.lockoutChecker != nil {
			if lerr := h.lockoutChecker.RecordFailedAttempt(r.Context(), user.ID); lerr != nil {
				slog.Warn("failed to record lockout attempt in Token", "err", lerr, "user_id", user.ID)
			}
		}
		// Record failed attempt when risk assessor is available.
		if h.deviceRiskAssessor != nil {
			fp := collectFingerprint(r)
			if recErr := h.deviceRiskAssessor.RecordLogin(r.Context(), user.ID, fp, false); recErr != nil {
				slog.Warn("failed to record failed Token attempt", "err", recErr)
			}
		}
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	actingTenant, actingRoles, err := h.resolveActingTenant(r.Context(), user, body.Tenant)
	if err != nil {
		respondTenantResolution(w, r, err)
		return
	}

	token, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.Email, actingRoles, actingTenant, user.TokenVersion)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeTokenSignFailed, nil)
		return
	}

	respond(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_in": h.expirySecs,
	})
}

func (h *AuthHandler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     security.SessionCookieNameFor(h.secureCook),
		Value:    token,
		Path:     "/",
		MaxAge:   int(h.expirySecs),
		HttpOnly: true,
		Secure:   h.secureCook,
		SameSite: http.SameSiteStrictMode,
	})
}

// generateCSRFToken creates a cryptographically random 32-byte CSRF token.
// Uses crypto/rand and base64url encoding (43 chars, no padding).
func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// setCSRFCookie sets the __Host-csrf cookie for the double-submit cookie pattern.
// The cookie is NOT HttpOnly so JavaScript can read it and send it as X-CSRF-Token.
// Uses __Host- prefix which requires Secure, Path=/, and no Domain (browser-enforced).
func (h *AuthHandler) setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     apimw.CSRFCookieNameFor(h.secureCook),
		Value:    token,
		Path:     "/",
		MaxAge:   int(h.expirySecs),
		HttpOnly: false,
		Secure:   h.secureCook,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearCSRFCookie removes the CSRF cookie (used on logout).
func (h *AuthHandler) clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     apimw.CSRFCookieNameFor(h.secureCook),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: false,
		Secure:   h.secureCook,
		SameSite: http.SameSiteStrictMode,
	})
}

// Refresh exchanges a refresh token for a new access token + refresh token pair.
// POST /api/admin/auth/refresh
// Body: {"refresh_token": "..."}
// Auth: public (rate-limited).
func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	if h.refreshTokenStore == nil {
		respondErrCode(w, r, http.StatusNotImplemented, i18n.CodeRefreshNotEnabled, nil)
		return
	}

	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil || body.RefreshToken == "" {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}

	rotated, err := h.refreshTokenStore.Rotate(r.Context(), body.RefreshToken, h.refreshTokenTTL)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrBackendUnavailable):
			// Nothing is known about the token, so 401 would be a lie that
			// costs the caller its session. 503 asks it to come back.
			respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		case errors.Is(err, auth.ErrTokenReuse):
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeTokenReuse, nil)
		case errors.Is(err, auth.ErrFamilyRevoked):
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeSessionRevoked, nil)
		case errors.Is(err, auth.ErrTokenExpired):
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeTokenExpired, nil)
		default:
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
		}
		return
	}

	// Load user data for the new access token claims.
	userID, err := uuid.Parse(rotated.UserID)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	user, err := h.users.GetByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Don't leak user existence via USER_NOT_FOUND.
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
			return
		}
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}

	// Account state enforcement
	// Use generic Unauthorized to prevent state enumeration.
	if user.Disabled {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
		return
	}
	if user.ExpiresAt != nil && time.Now().After(*user.ExpiresAt) {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
		return
	}

	// A family issued before the account's token_version moved (logout on any
	// device, a disable, a password set) ends here. Signing with the live
	// version would otherwise hand it a session the bump was meant to end,
	// and logout revokes only the family it is shown.
	if rotated.TokenVersion < user.TokenVersion {
		if err := h.refreshTokenStore.RevokeFamily(r.Context(), rotated.FamilyID); err != nil {
			slog.Warn("revoke refresh family issued under an older token version", "err", err)
		}
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
		return
	}

	// Acting tenant
	// The family record remembers which tenant the session was acting in.
	// Signing user.TenantID here instead would move anyone who had switched
	// tenants back to their home one at the first refresh, with the home roles
	// attached: a privilege change nobody asked for, on a timer.
	//
	// Re-resolving rather than replaying the stored value re-checks the
	// membership, so revoking someone's access to a tenant ends their session
	// there at the next refresh instead of at token expiry.
	actingTenant, actingRoles, err := h.resolveActingTenant(r.Context(), user, rotated.TenantID)
	if err != nil {
		respondTenantResolution(w, r, err)
		return
	}

	accessToken, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.Email, actingRoles, actingTenant, user.TokenVersion)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeTokenSignFailed, nil)
		return
	}

	csrfToken, err := generateCSRFToken()
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	h.setSessionCookie(w, accessToken)
	h.setCSRFCookie(w, csrfToken)
	respond(w, http.StatusOK, map[string]any{
		"token":         accessToken,
		"refresh_token": rotated.RefreshToken,
		"csrf_token":    csrfToken,
		"expires_in":    h.expirySecs,
	})
}

// safeUser strips password_hash from the response.
func safeUser(u *domain.User) map[string]any {
	out := map[string]any{
		"id":         u.ID.String(),
		"email":      u.Email,
		"roles":      u.Roles,
		"tenant_id":  u.TenantID,
		"disabled":   u.Disabled,
		"created_at": u.CreatedAt,
	}
	if u.ExpiresAt != nil {
		out["expires_at"] = u.ExpiresAt
	}
	return out
}
