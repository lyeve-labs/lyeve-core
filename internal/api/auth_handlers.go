package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
	"github.com/lyeve-labs/lyeve-core/pkg/security/encryption"
)

// SetupStatus reports whether the admin account has been created and, while
// it has not, where the operator finds the setup token: "env" when
// LYEVE_SETUP_TOKEN is set, "log" when the engine printed one at boot, and
// absent when this process holds no token (setup is then refused until the
// engine restarts with one).
// GET /api/admin/setup
// Auth: public.
func (h *AuthHandler) SetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := h.users.Count(r.Context())
	if err != nil {
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}
	body := map[string]any{"setup_required": n == 0}
	if n == 0 {
		if src := h.setupToken.Source(); src != "" {
			body["token_source"] = src
		}
	}
	respond(w, http.StatusOK, body)
}

// Setup creates the first super_admin account. Only works when no users
// exist, and only for a caller holding the setup token: the route is public
// because no account exists yet, so the token is the one thing separating the
// operator from anyone else who can reach the admin listener.
// POST /api/admin/setup
// Body: {"email": "...", "password": "...", "setup_token": "..."}
// The token may come in the X-Setup-Token header instead.
// Auth: setup token.
func (h *AuthHandler) Setup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}
	presented := body.SetupToken
	if presented == "" {
		presented = r.Header.Get(SetupTokenHeader)
	}
	if !h.setupToken.Matches(presented) {
		slog.Warn("setup: refused a first-admin claim without a valid setup token",
			"remote", r.RemoteAddr, "token_presented", presented != "")
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeSetupTokenInvalid, nil)
		return
	}
	if body.Email == "" {
		respondErrCode(w, r, http.StatusUnprocessableEntity, i18n.CodeValidationRequired, nil)
		return
	}
	if err := auth.ValidatePassword(h.passwordPolicy, body.Password); err != nil {
		code, params := passwordViolationResponse(err, h.passwordPolicy)
		respondErrCode(w, r, http.StatusUnprocessableEntity, code, params)
		return
	}

	// A cheap refusal before the password hash. The store repeats the check
	// under a lock, which is the one that decides.
	n, err := h.users.Count(r.Context())
	if err != nil {
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}
	if n > 0 {
		h.setupToken.Retire()
		respondErrCode(w, r, http.StatusConflict, i18n.CodeSetupAlreadyComplete, nil)
		return
	}

	// In a multi-tenant deployment the bootstrap super_admin needs a tenant to
	// belong to. Provision the default tenant and stamp the account with it.
	tenantID := ""
	if h.multiTenant {
		if err := h.ensureDefaultTenant(r.Context()); err != nil {
			respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
			return
		}
		tenantID = "default"
	}

	hash, err := auth.HashPassword(h.passwordHashAlgo, body.Password)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodePasswordHashFailed, nil)
		return
	}

	user, err := h.users.CreateFirstAdmin(r.Context(), body.Email, hash, tenantID)
	if errors.Is(err, db.ErrSetupComplete) {
		h.setupToken.Retire()
		respondErrCode(w, r, http.StatusConflict, i18n.CodeSetupAlreadyComplete, nil)
		return
	}
	if err != nil {
		respondErrCode(w, r, httpx.StoreStatusFor(err), i18n.CodeUserCreateFailed, nil)
		return
	}
	h.setupToken.Retire()
	slog.Info("setup: first super admin created; the setup token no longer opens setup", "user_id", user.ID)

	token, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.Email, user.Roles, user.TenantID, user.TokenVersion)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeTokenSignFailed, nil)
		return
	}

	csrfToken, err := generateCSRFToken()
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	h.setSessionCookie(w, token)
	h.setCSRFCookie(w, csrfToken)
	respond(w, http.StatusCreated, map[string]any{
		"user":       safeUser(user),
		"token":      token,
		"csrf_token": csrfToken,
	})
}

// ensureDefaultTenant registers the default tenant for a multi-tenant first
// run, so the bootstrap super_admin belongs somewhere rather than being
// stamped with a tenant nothing has heard of.
//
// The roster belongs to the plugin that supplies it, so the engine asks that
// plugin to register the row instead of writing it.
//
// Nil on an install with no roster supplier, where there is nothing to
// register. Setup then creates the account and leaves it at that.
func (h *AuthHandler) ensureDefaultTenant(ctx context.Context) error {
	if h.defaultTenant == nil {
		return nil
	}
	if err := h.defaultTenant(ctx); err != nil {
		return fmt.Errorf("provision default tenant: %w", err)
	}
	return nil
}

// warnIfTenantMissing reports an account whose tenant is not on the roster.
// The line names the tenant and the account's subject_ref. Log lines are
// stored, so the address appears as its stable digest and never as itself.
//
// TenantHeader validates the X-Tenant-ID override against the roster and does
// not validate the claim the credential carries, so a user whose sys_users row
// names a tenant that does not exist signs in, resolves that scope, and reads
// nothing: every tenant-scoped query carries `AND tenant_id = $N` and matches
// no rows. Nothing errors, so the account looks empty rather than misconfigured.
//
// It happens. Only /api/admin/setup provisions the default tenant, and only
// while sys_users is empty, so any install bootstrapped another way can end up
// with this shape.
//
// Diagnostic only, and deliberately not a refusal: an operator who has lost the
// row needs to sign in to put it back. Checked here rather than in TenantHeader
// because the roster read is a query, and login is rate-limited while every
// request is not.
//
// Single-tenant installs are skipped. They resolve the implicit tenant without
// ever provisioning a row for it, so the absence means nothing there.
func (h *AuthHandler) warnIfTenantMissing(ctx context.Context, tenant, email string) {
	if !h.multiTenant || tenant == "" || h.tenantValidator == nil {
		return
	}
	if h.tenantValidator(ctx, tenant) {
		return
	}
	slog.WarnContext(ctx, "account is scoped to a tenant that is not on the roster",
		"subject_ref", compliance.SubjectRef(email), "tenant", tenant,
		"hint", "every tenant-scoped read will return nothing; register the tenant or re-scope the account")
}

// hasControlChars reports whether s holds a C0 control character or DEL. No
// stored email address contains one, and passing one to the database is a
// driver error rather than a lookup.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Login authenticates a user and issues a JWT via cookie + response body.
//
// Security properties:
//   - Enumeration resistance: unknown emails receive a dummy bcrypt comparison
//     against a constant hash, producing identical timing and error messages.
//   - Account lockout: after MaxFailedAttempts consecutive failures from the
//     same user (tracked by the LockoutChecker), returns HTTP 429.
//   - MFA fail-closed: if mfaStore.IsEnabled errors, login is denied rather
//     than issuing an unauthenticated session.
//   - Account state: disabled or expired accounts are rejected before session
//     issuance.
//
// POST /api/admin/auth/login
// Body: {"email": "...", "password": "..."}
// Auth: public (rate-limited).
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		// Tenant names which of the caller's tenants this session acts in.
		// Empty means their home tenant, which is every single-tenant login.
		Tenant string `json:"tenant"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}

	// A control character cannot appear in a stored address, so this is an
	// unknown email however the driver would react to it. Postgres rejects a
	// NUL in a text parameter outright, so the lookup is skipped and the
	// request gets the dummy compare and the answer any unknown email gets.
	if hasControlChars(body.Email) {
		_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	user, err := h.users.GetByEmail(r.Context(), body.Email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Enumeration prevention: unknown email -> dummy bcrypt compare.
			// Always runs bcrypt.CompareHashAndPassword against a constant
			// hash so the timing is identical to a known-email+wrong-password
			// path. The error message is the same generic text.
			_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}

	// Account lockout check (before password verification)
	//
	// Refusing here has to look exactly like a wrong password, or the lockout
	// becomes the enumeration oracle the rest of this handler is built to
	// avoid: six failed attempts against an address, and a distinct code, a
	// distinct status, or a fast answer all confirm the address is real. Only
	// an account that exists can be locked, so any difference is a direct
	// existence signal.
	//
	// The dummy compare is what closes the timing half. Without it this path
	// answers in under two milliseconds against roughly fifty for every other
	// rejection, which is measurable across a network.
	if h.lockoutChecker != nil {
		count, lerr := h.lockoutChecker.CountRecentFailures(r.Context(), user.ID, time.Now().Add(-h.LockoutWindow))
		if lerr != nil {
			slog.Warn("lockout check failed, failing closed", "err", lerr, "user_id", user.ID)
			_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
		if count >= h.MaxFailedAttempts {
			// The response above is deliberately indistinguishable from a wrong
			// password, so a locked account is invisible to the caller. It must
			// not be invisible to the operator as well: the lock lives in memory
			// or a cache backend, nothing in the database reflects it, and the
			// only outward sign is a 401 that reads as a bad credential. One
			// line names it for the operator and changes no response.
			slog.WarnContext(r.Context(), "login refused: account is locked out",
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
		// A failed password check has to reach the operator. Five of them in the
		// window lock the account, and the refusal after that is
		// indistinguishable from this one, so without this line a lockout
		// investigation could see neither what locked the account nor what spent
		// its attempts.
		//
		// Info, not Warn: a mistyped password is ordinary traffic and a WARN per
		// typo buries the lines that matter. No email and no credential, only
		// the id the lockout counts against, whether a credential arrived at all
		// (an empty one means a truncated or malformed body rather than a wrong
		// password) and whether the stored hash was present.
		slog.InfoContext(r.Context(), "login failed: password did not verify",
			"user_id", user.ID,
			"credential_supplied", body.Password != "",
			"hash_present", user.PasswordHash != "",
			"algo", h.passwordHashAlgo)
		// Record failed attempt for lockout enforcement.
		if h.lockoutChecker != nil {
			if lerr := h.lockoutChecker.RecordFailedAttempt(r.Context(), user.ID); lerr != nil {
				slog.Warn("failed to record lockout attempt", "err", lerr)
			}
		}
		// Record failed attempt when risk assessor is available.
		if h.deviceRiskAssessor != nil {
			fp := collectFingerprint(r)
			if recErr := h.deviceRiskAssessor.RecordLogin(r.Context(), user.ID, fp, false); recErr != nil {
				slog.Warn("failed to record login attempt", "err", recErr)
			}
		}
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	// Account state enforcement (after password, before session)
	// Answer exactly as a wrong password does. Collapsing disabled into
	// expired is not enough on its own: a caller holding the correct password
	// still learns the account exists and is locked if the code differs from
	// the credentials-failure path.
	if user.Disabled {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}
	if user.ExpiresAt != nil && time.Now().After(*user.ExpiresAt) {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	// Acting tenant
	// One account can hold several tenants, so the session has to name one
	// before any token is signed. Resolving here, above the MFA branch, means
	// the challenge token and the session token cannot disagree about which
	// tenant the login is for.
	//
	// A caller who is not a member is answered exactly as a wrong password is.
	// Anything else turns login into a probe for which tenants an address
	// belongs to, which is the enumeration property the rest of this handler
	// spends real effort protecting.
	actingTenant, actingRoles, err := h.resolveActingTenant(r.Context(), user, body.Tenant)
	if err != nil {
		respondTenantResolution(w, r, err)
		return
	}

	// Device risk assessment (before MFA check: risk may escalate)
	fp := collectFingerprint(r)
	var risk *security.RiskAssessment
	enforceMFA := false
	if h.deviceRiskAssessor != nil {
		var raErr error
		risk, raErr = h.deviceRiskAssessor.Assess(r.Context(), user.ID, fp)
		if raErr != nil {
			slog.Warn("risk assessment failed, failing closed: enforcing MFA", "err", raErr)
			if !h.riskBasedMFADisabled {
				enforceMFA = true
			}
		} else if risk != nil && risk.RequireMFA && !h.riskBasedMFADisabled {
			enforceMFA = true
		}
		if risk != nil && risk.RequireAlert {
			slog.Warn("suspicious login detected",
				"user_id", user.ID,
				"subject_ref", compliance.SubjectRef(user.Email),
				"ip", fp.IP,
				"score", risk.Score,
				"reason", risk.Reason,
			)
		}
	}

	mfaEnabled := false
	if h.mfaStore != nil {
		var mfaErr error
		mfaEnabled, mfaErr = h.mfaStore.IsEnabled(r.Context(), user.ID)
		if mfaErr != nil {
			// MFA fail-closed: deny login instead of issuing a session.
			slog.Warn("mfa check query failed, failing closed: denying login", "err", mfaErr)
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeMFAStoreError, nil)
			return
		}
	}

	// Risk escalation can only escalate to a factor the user actually holds.
	// A challenge issued to an account with nothing enrolled is unsatisfiable:
	// mfa-verify answers MFA_NOT_CONFIGURED for every code, and every
	// enrollment route needs the session the challenge is withholding. A new
	// device can score over the escalation threshold, and escalating to a
	// factor the account does not hold is not a control, so record the risk
	// and let the session through. Accounts that did enroll are still
	// challenged below via mfaEnabled.
	if enforceMFA && !mfaEnabled {
		score := 0
		if risk != nil {
			score = risk.Score
		}
		slog.Warn("risk escalation wanted MFA but the account has none enrolled, allowing login",
			"user_id", user.ID, "subject_ref", compliance.SubjectRef(user.Email), "ip", fp.IP, "score", score)
		enforceMFA = false
	}

	// Enforce MFA when risk assessment demands it OR user has MFA enabled
	if enforceMFA || mfaEnabled {
		// Issue challenge token: even if the user didn't opt into MFA, risk escalation forces it.
		challengeToken, err := auth.SignChallenge(h.jwtSecret, user.ID, user.Email, actingRoles, actingTenant)
		if err != nil {
			respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeChallengeFailed, nil)
			return
		}
		resp := map[string]any{
			"mfa_required":    true,
			"challenge_token": challengeToken,
		}

		// Advertise available MFA methods.
		methods := []string{"totp"}
		if h.mfaStore != nil {
			hasWA, err := h.mfaStore.HasWebAuthn(r.Context(), user.ID)
			if err == nil && hasWA {
				methods = append(methods, "webauthn")
			}
		}
		resp["mfa_methods"] = methods

		if risk != nil {
			resp["risk"] = risk
		}
		respond(w, http.StatusOK, resp)
		return
	}

	h.warnIfTenantMissing(r.Context(), actingTenant, user.Email)

	// Login: no MFA required, issue full session token
	token, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.Email, actingRoles, actingTenant, user.TokenVersion)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeTokenSignFailed, nil)
		return
	}

	csrfToken, err := generateCSRFToken()
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	resp := map[string]any{
		"user":       safeUser(user),
		"token":      token,
		"csrf_token": csrfToken,
	}

	// Issue refresh token if store is configured.
	if h.refreshTokenStore != nil {
		rt, err := h.refreshTokenStore.IssueForSession(r.Context(), user.ID.String(), actingTenant, user.TokenVersion, h.refreshTokenTTL)
		if err != nil {
			slog.Error("failed to issue refresh token, continuing without", "err", err)
		} else {
			resp["refresh_token"] = rt.RefreshToken
		}
	}

	// Record successful login.
	if h.deviceRiskAssessor != nil {
		// Pass existing risk assessment via context to avoid a duplicate Assess call.
		recCtx := r.Context()
		if risk != nil {
			recCtx = context.WithValue(recCtx, security.RiskAssessmentCtxKey, risk)
		}
		if recErr := h.deviceRiskAssessor.RecordLogin(recCtx, user.ID, fp, true); recErr != nil {
			slog.Warn("failed to record login attempt", "err", recErr)
		}
	}

	if risk != nil {
		resp["risk"] = risk
	}
	h.setSessionCookie(w, token)
	h.setCSRFCookie(w, csrfToken)
	respond(w, http.StatusOK, resp)
}

// mfaDefaultTenantKeyID is the key id a TOTP secret is sealed under when the
// enrolling session carries no tenant. Reading the secret back has to use the
// same id or the derived DEK is a different key.
const mfaDefaultTenantKeyID = "default"

// MFAVerify completes the MFA challenge: validates a challenge token + TOTP code
// and issues a full session JWT on success.
// POST /api/admin/auth/mfa-verify
// Body: {"challenge_token": "...", "code": "..."}
// Auth: public (rate-limited).
func (h *AuthHandler) MFAVerify(w http.ResponseWriter, r *http.Request) {
	// When no MFA store is wired, return 501 so the caller degrades
	// gracefully rather than panicking.
	if h.mfaStore == nil {
		respondErrCode(w, r, http.StatusNotImplemented, i18n.CodeMFANotAvailable, nil)
		return
	}

	var body struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil || body.ChallengeToken == "" || body.Code == "" {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}

	// Validate the challenge token: must be valid, non-expired, and MFAPending.
	claims, err := auth.ParseMulti([]string{h.jwtSecret}, body.ChallengeToken)
	if err != nil || !claims.MFAPending {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeChallengeExpired, nil)
		return
	}

	// Challenge single-use enforcement
	// Reject replayed challenges: if this token's jti has already been
	// consumed by a successful MFA verification, treat it as expired.
	// A state that cannot be read counts as used: an outage of a shared
	// lockout backend must not reopen a challenge another replica consumed.
	if claims.ID != "" {
		used, jerr := h.mfaLockoutChecker.IsJTIUsed(r.Context(), claims.ID)
		if jerr != nil {
			slog.WarnContext(r.Context(), "mfa challenge replay state unreadable, refusing the challenge", "err", jerr)
		}
		if used || jerr != nil {
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeChallengeExpired, nil)
			return
		}
	}

	// Load encrypted TOTP secret via MFA store.
	userID, err := uuid.Parse(claims.UserID)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	// MFA lockout checks
	// Check per-user lockout first, then challenge-level exhaustion.
	// Both must pass before TOTP verification proceeds.
	challengeHash := sha256Hex(body.ChallengeToken)
	// A limit that cannot be read fails closed with a retryable 503, the way
	// the password path refuses when its counter is unreachable: letting codes
	// through while the counters are down would lift the limit for as long as
	// the outage lasts.
	locked, lerr := h.mfaLockoutChecker.CheckMFALockout(r.Context(), userID)
	if lerr != nil {
		slog.WarnContext(r.Context(), "mfa lockout check failed, failing closed", "err", lerr, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		return
	}
	if locked {
		respondErrCode(w, r, http.StatusTooManyRequests, i18n.CodeMFALockedOut, nil)
		return
	}
	exhausted, cerr := h.mfaLockoutChecker.CheckChallengeLimit(r.Context(), challengeHash)
	if cerr != nil {
		slog.WarnContext(r.Context(), "mfa challenge limit check failed, failing closed", "err", cerr, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		return
	}
	if exhausted {
		respondErrCode(w, r, http.StatusTooManyRequests, i18n.CodeMFALockedOut, nil)
		return
	}
	// Record the challenge attempt: even a valid token consumes
	// one of the allowed attempts. Done before TOTP verification so
	// the attempt is counted regardless of the outcome.
	if rerr := h.mfaLockoutChecker.RecordChallengeAttempt(r.Context(), challengeHash); rerr != nil {
		slog.WarnContext(r.Context(), "failed to record mfa challenge attempt", "err", rerr, "user_id", userID)
	}

	encSecret, backupHashes, err := h.mfaStore.GetEnabled(r.Context(), userID)
	if err != nil {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeMFANotConfigured, nil)
		return
	}

	plainSecret, err := h.totpSecret(r.Context(), userID, claims.TenantID, encSecret)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	outcome, terr := acceptTOTP(r.Context(), h.mfaStore, userID, plainSecret, body.Code)
	if terr != nil {
		slog.WarnContext(r.Context(), "mfa code check failed, failing closed", "err", terr, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		return
	}
	// A replayed code falls through to the recovery code check like any other
	// code that does not sign in, and fails there as an invalid code.
	if outcome != totpAccepted {
		// Check backup codes as fallback.
		codeHash := func(c string) string {
			h2 := sha256.Sum256([]byte(c))
			return hex.EncodeToString(h2[:])
		}(body.Code)
		remaining := make([]string, 0, len(backupHashes))
		found := false
		for _, h2 := range backupHashes {
			if subtle.ConstantTimeCompare([]byte(h2), []byte(codeHash)) == 1 {
				found = true
			} else {
				remaining = append(remaining, h2)
			}
		}
		if !found {
			// Record the user-level failure so repeated bad codes
			// trigger per-user lockout.
			if rerr := h.mfaLockoutChecker.RecordMFAFailure(r.Context(), userID); rerr != nil {
				slog.WarnContext(r.Context(), "failed to record mfa failure", "err", rerr, "user_id", userID)
			}
			respondErrCode(w, r, http.StatusUnprocessableEntity, i18n.CodeMFAInvalidCode, nil)
			return
		}
		// Consume the used backup code.
		if err := h.mfaStore.UpdateBackupCodes(r.Context(), userID, remaining); err != nil {
			slog.Error("mfa: persist backup code consumption failed", "user_id", userID, "err", err)
		}
	}

	// Re-fetch the live user to get current roles, disabled, and expiry state.
	// The challenge token's claims may be stale if the user was modified after
	// the challenge was issued (it's only 5-minute TTL, but still possible).
	var (
		liveEmail    string
		liveRoles    []string
		liveTenantID string
		liveUser     *domain.User
	)
	if h.users != nil {
		if u, err := h.users.GetByID(r.Context(), userID); err == nil {
			liveUser = u
			liveEmail = u.Email
			liveRoles = u.Roles
			liveTenantID = u.TenantID

			// Account state enforcement
			// Use generic Unauthorized to prevent state enumeration.
			if u.Disabled {
				respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
				return
			}
			if u.ExpiresAt != nil && time.Now().After(*u.ExpiresAt) {
				respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeUnauthorized, nil)
				return
			}
		}
	}
	if liveEmail == "" {
		liveEmail = claims.Email
	}
	if len(liveRoles) == 0 {
		liveRoles = claims.Roles
	}
	if liveTenantID == "" {
		liveTenantID = claims.TenantID
	}

	// Re-resolve the acting tenant
	// The challenge token carries the tenant the login chose. Reading it back
	// from the user row instead, as the block above does, would drop a caller
	// who logged into their second tenant back to their home one the moment
	// they passed MFA: a silent tenant switch, with the roles to match.
	//
	// Re-resolving rather than trusting the claim is what makes this a check.
	// A membership revoked between the challenge and this call is caught here,
	// and a forged tenant in a challenge token has to survive the same
	// membership lookup the login did.
	if liveUser != nil {
		tenantID, roles, rerr := h.resolveActingTenant(r.Context(), liveUser, claims.TenantID)
		if rerr != nil {
			respondTenantResolution(w, r, rerr)
			return
		}
		liveTenantID, liveRoles = tenantID, roles
	}

	// Re-fetch token_version. Fall back to 0 (safe for 5-min challenge TTL).
	liveTokenVersion := 0
	if h.users != nil {
		if u, err := h.users.GetByID(r.Context(), userID); err == nil {
			liveTokenVersion = u.TokenVersion
		}
	}

	// Issue a full session JWT with current user data.
	token, err := auth.Sign(h.jwtSecret, h.expirySecs, userID, liveEmail, liveRoles, liveTenantID, liveTokenVersion)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeTokenSignFailed, nil)
		return
	}

	csrfToken, err := generateCSRFToken()
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	resp := map[string]any{
		"token":      token,
		"csrf_token": csrfToken,
	}

	// Issue refresh token if store is configured.
	if h.refreshTokenStore != nil {
		rt, err := h.refreshTokenStore.IssueForSession(r.Context(), userID.String(), liveTenantID, liveTokenVersion, h.refreshTokenTTL)
		if err != nil {
			slog.Error("failed to issue refresh token in mfa-verify, continuing without", "err", err)
		} else {
			resp["refresh_token"] = rt.RefreshToken
		}
	}

	h.setSessionCookie(w, token)
	h.setCSRFCookie(w, csrfToken)

	// Mark the challenge JTI as consumed so this token cannot be replayed.
	if merr := h.mfaLockoutChecker.MarkJTIUsed(r.Context(), claims.ID); merr != nil {
		slog.WarnContext(r.Context(), "failed to mark mfa challenge used", "err", merr, "user_id", userID)
	}

	respond(w, http.StatusOK, resp)
}

// totpSecret decrypts a user's stored TOTP secret.
//
// Secrets are sealed under the DEK of the enrolling tenant, or under
// mfaDefaultTenantKeyID when the enrolling session carried none, so reading
// one back has to derive the same key. claimsTenant is the tenant the
// caller's token names, used when the account records none.
//
// A secret that does not open under the tenant key is read with
// security.DecryptSecret, which keys on SHA-256 of the JWT secret.
func (h *AuthHandler) totpSecret(ctx context.Context, userID uuid.UUID, claimsTenant, encSecret string) (string, error) {
	ks := h.keyStore
	if ks == nil {
		legacy, err := encryption.NewKeyStoreLegacy(h.jwtSecret)
		if err != nil {
			return "", fmt.Errorf("totp secret: key store: %w", err)
		}
		ks = legacy
	}
	tenantID := claimsTenant
	if h.users != nil {
		if u, uErr := h.users.GetByID(ctx, userID); uErr == nil && u.TenantID != "" {
			tenantID = u.TenantID
		}
	}
	if tenantID == "" {
		tenantID = mfaDefaultTenantKeyID
	}
	dek, err := ks.DeriveTenantDEK(tenantID)
	if err != nil {
		return "", fmt.Errorf("totp secret: derive key: %w", err)
	}
	plain, decErr := encryption.DecryptValue(dek, encSecret)
	if decErr == nil {
		return string(plain), nil
	}
	legacyVal, err := security.DecryptSecret(encSecret, h.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("totp secret: decrypt: %w", err)
	}
	return legacyVal, nil
}

// totpSecretAny decrypts a TOTP secret sealed under any one of tenants,
// trying each in turn, then the legacy key. A wrong tenant fails the GCM tag
// rather than yielding a wrong secret, so trying several is safe.
func (h *AuthHandler) totpSecretAny(tenants []string, encSecret string) (string, error) {
	ks := h.keyStore
	if ks == nil {
		legacy, err := encryption.NewKeyStoreLegacy(h.jwtSecret)
		if err != nil {
			return "", fmt.Errorf("totp secret: key store: %w", err)
		}
		ks = legacy
	}
	for _, t := range tenants {
		dek, err := ks.DeriveTenantDEK(t)
		if err != nil {
			continue
		}
		if plain, err := encryption.DecryptValue(dek, encSecret); err == nil {
			return string(plain), nil
		}
	}
	legacyVal, err := security.DecryptSecret(encSecret, h.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("totp secret: decrypt: %w", err)
	}
	return legacyVal, nil
}
