package api

import (
	"context"
	"crypto/sha256"
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
	"github.com/lyeve-labs/lyeve-core/pkg/core"
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
		code, params := auth.PasswordViolationCode(err, h.passwordPolicy)
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
	// The line is about the account, so it goes under the tenant the account
	// signs in to. A host whose tenant differs, such as one mapped to the
	// default tenant, would otherwise show that tenant's admins the account's
	// digest and tenant.
	slog.WarnContext(accountContext(ctx, tenant), "account is scoped to a tenant that is not on the roster",
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
//   - MFA fail-closed: if mfaStore.IsEnabled errors, login is denied with a
//     503 rather than issuing an unauthenticated session. The password may
//     well be right, so the answer is not a credential failure.
//   - Account state: disabled or expired accounts are rejected before session
//     issuance.
//
// POST /api/admin/auth/login
// Body: {"email": "...", "password": "..."}
// Auth: public (rate-limited).
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	attempt := watchSignIn(w, "password")
	w = attempt
	defer h.auditSignIn(r, attempt)
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
	attempt.email = body.Email

	// A control character cannot appear in a stored address, so this is an
	// unknown email however the driver would react to it. Postgres rejects a
	// NUL in a text parameter outright, so the lookup is skipped and the
	// request gets the dummy compare and the answer any unknown email gets.
	if hasControlChars(body.Email) {
		_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}

	user, err := h.passwordAccount(r.Context(), body.Email, body.Tenant)
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
	attempt.account, attempt.tenant = user.ID, user.TenantID

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
	//
	// The attempt is counted here, before the password is checked, in the
	// same step that reads the count, so concurrent wrong passwords cannot all
	// read a count under the limit and all be checked. A correct password
	// clears the count below.
	if h.lockoutChecker != nil {
		count, reserved, lerr := h.lockoutChecker.ReserveAttempt(r.Context(), user.ID, h.MaxFailedAttempts)
		if lerr != nil {
			slog.Warn("lockout check failed, failing closed", "err", lerr, "user_id", user.ID)
			_ = auth.VerifyPassword(h.passwordHashAlgo, auth.DummyBcryptHash, body.Password) // err suppressed: timing-attack mitigation, result intentionally discarded
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
			return
		}
		if !reserved {
			// The response above is deliberately indistinguishable from a wrong
			// password, so a locked account is invisible to the caller. It must
			// not be invisible to the operator as well: the lock lives in memory
			// or a cache backend, nothing in the database reflects it, and the
			// only outward sign is a 401 that reads as a bad credential. One
			// line names it for the operator and changes no response.
			slog.WarnContext(failedAttemptContext(r.Context(), user), "login refused: account is locked out",
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
		attemptCtx := failedAttemptContext(r.Context(), user)
		slog.InfoContext(attemptCtx, "login failed: password did not verify",
			"user_id", user.ID,
			"credential_supplied", body.Password != "",
			"hash_present", user.PasswordHash != "",
			"algo", h.passwordHashAlgo)
		// Record failed attempt when risk assessor is available.
		if h.deviceRiskAssessor != nil {
			fp := collectFingerprint(r)
			if recErr := h.deviceRiskAssessor.RecordLogin(attemptCtx, user.ID, fp, false); recErr != nil {
				slog.Warn("failed to record login attempt", "err", recErr)
			}
		}
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeInvalidCredentials, nil)
		return
	}
	h.clearPasswordAttempts(r.Context(), user.ID)

	// Account state enforcement (after password, before session)
	// Answer exactly as a wrong password does. Collapsing disabled into
	// expired is not enough on its own: a caller holding the correct password
	// still learns the account exists and is locked if the code differs from
	// the credentials-failure path.
	if !h.respondIfPasswordSignInRefused(w, r, user) {
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
	actingTenant, actingRoles, err := h.resolveSignInTenant(r.Context(), user, body.Tenant)
	if err != nil {
		respondTenantResolution(w, r, err)
		return
	}
	attempt.tenant = actingTenant

	fp := collectFingerprint(r)
	risk, answered := h.respondIfSecondFactorRequired(w, r, user, fp, actingRoles, actingTenant)
	if answered {
		return
	}

	h.warnIfTenantMissing(r.Context(), actingTenant, user.Email)

	// Login: no MFA required, issue full session token
	token, err := auth.Sign(h.jwtSecret, h.expirySecs, user.ID, user.SignInEmail(), actingRoles, actingTenant, user.TokenVersion)
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
	attempt.issued, attempt.tenant = true, actingTenant
	h.setSessionCookie(w, token)
	h.setCSRFCookie(w, csrfToken)
	respond(w, http.StatusOK, resp)
}

// respondIfSecondFactorRequired runs the second-factor decision every password
// sign-in shares, after the password, the account state and the acting tenant
// have passed. An account with a factor enrolled, or one that risk scoring
// escalates, gets a challenge token in place of a session, and a factor store
// that cannot be read refuses the sign-in. When it answers the request it
// returns true, and the caller issues nothing. Otherwise it returns the risk
// assessment, which the caller records with the login.
func (h *AuthHandler) respondIfSecondFactorRequired(w http.ResponseWriter, r *http.Request, user *domain.User, fp security.DeviceFingerprint, actingRoles []string, actingTenant string) (*security.RiskAssessment, bool) {
	// Device risk assessment (before MFA check: risk may escalate)
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
			// MFA fail-closed: deny login instead of issuing a session. The
			// store failed, not the credentials, so the answer is a 503 the
			// client retries, as mfa-verify answers the same outage.
			slog.Warn("mfa check query failed, failing closed: denying login", "err", mfaErr)
			respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeMFAStoreError, nil)
			return risk, true
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
		challengeToken, err := auth.SignChallenge(h.jwtSecret, user.ID, user.SignInEmail(), actingRoles, actingTenant)
		if err != nil {
			respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeChallengeFailed, nil)
			return risk, true
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
		return risk, true
	}
	return risk, false
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
	attempt := watchSignIn(w, "second_factor")
	w = attempt
	defer h.auditSignIn(r, attempt)
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
	// The login that issued the challenge acted in the tenant of the host it
	// arrived on. A challenge for another tenant presented on a tenant's
	// mapped domain is not this host's, so it is refused before the second
	// factor is read or spent.
	if host := signInHostTenant(r.Context()); host != "" && claims.TenantID != host {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeChallengeExpired, nil)
		return
	}

	// The lines below are about the account behind the challenge. On a host
	// that names no tenant of its own, such as one mapped to the default
	// tenant, the request's tenant can be another, so they go under the
	// tenant the challenge was issued for.
	logCtx := accountContext(r.Context(), claims.TenantID)

	// Challenge single-use enforcement
	// Reject replayed challenges: if this token's jti has already been
	// consumed by a successful MFA verification, treat it as expired.
	// A state that cannot be read counts as used: an outage of a shared
	// lockout backend must not reopen a challenge another replica consumed.
	if claims.ID != "" {
		used, jerr := h.mfaLockoutChecker.IsJTIUsed(r.Context(), claims.ID)
		if jerr != nil {
			slog.WarnContext(logCtx, "mfa challenge replay state unreadable, refusing the challenge", "err", jerr)
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
	attempt.account, attempt.tenant = userID, claims.TenantID

	// MFA lockout checks
	// Check per-user lockout first, then spend one of the challenge's
	// attempts. Both must pass before TOTP verification proceeds, and the
	// user's attempt is counted again, atomically, just before the code check.
	challengeHash := sha256Hex(body.ChallengeToken)
	// A limit that cannot be read fails closed with a retryable 503, the way
	// the password path refuses when its counter is unreachable: letting codes
	// through while the counters are down would lift the limit for as long as
	// the outage lasts.
	locked, lerr := h.mfaLockoutChecker.CheckMFALockout(r.Context(), userID)
	if lerr != nil {
		slog.WarnContext(logCtx, "mfa lockout check failed, failing closed", "err", lerr, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		return
	}
	if locked {
		respondErrCode(w, r, http.StatusTooManyRequests, i18n.CodeMFALockedOut, nil)
		return
	}
	// Even a valid code consumes one of the challenge's attempts. The attempt
	// is spent in the same step that checks the cap, so concurrent requests on
	// one challenge cannot all find it unspent.
	spent, cerr := h.mfaLockoutChecker.ReserveChallengeAttempt(r.Context(), challengeHash)
	if cerr != nil {
		slog.WarnContext(logCtx, "mfa challenge limit check failed, failing closed", "err", cerr, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		return
	}
	if !spent {
		respondErrCode(w, r, http.StatusTooManyRequests, i18n.CodeMFALockedOut, nil)
		return
	}

	encSecret, _, err := h.mfaStore.GetEnabled(r.Context(), userID)
	if errors.Is(err, core.ErrNotFound) {
		respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeMFANotConfigured, nil)
		return
	}
	if err != nil {
		slog.WarnContext(logCtx, "mfa secret read failed", "err", err, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeMFAStoreError, nil)
		return
	}

	plainSecret, err := h.totpSecret(r.Context(), userID, claims.TenantID, encSecret)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodeInternalError, nil)
		return
	}

	// The user's attempt is counted before the code is checked, in the same
	// step that reads the count, so concurrent wrong codes cannot all read a
	// count under the cap and all be checked. A code that verifies clears it.
	reserved, rerr := h.mfaLockoutChecker.ReserveMFAAttempt(r.Context(), userID)
	if rerr != nil {
		slog.WarnContext(logCtx, "mfa lockout check failed, failing closed", "err", rerr, "user_id", userID)
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
		return
	}
	if !reserved {
		respondErrCode(w, r, http.StatusTooManyRequests, i18n.CodeMFALockedOut, nil)
		return
	}

	outcome, terr := acceptTOTP(r.Context(), h.mfaStore, userID, plainSecret, body.Code)
	if terr != nil {
		slog.WarnContext(logCtx, "mfa code check failed, failing closed", "err", terr, "user_id", userID)
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
		// A recovery code is spent before any session is signed, by the
		// store's one-step consume, so two requests cannot both spend it. A
		// spend that cannot be written refuses the sign-in.
		found, spendErr := h.mfaStore.ConsumeBackupCode(r.Context(), userID, codeHash)
		if spendErr != nil {
			slog.WarnContext(logCtx, "mfa recovery code spend failed, failing closed", "err", spendErr, "user_id", userID)
			respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
			return
		}
		if !found {
			// The attempt reserved above stays counted, so repeated bad
			// codes trigger per-user lockout.
			respondErrCode(w, r, http.StatusUnprocessableEntity, i18n.CodeMFAInvalidCode, nil)
			return
		}
	}
	if cerr := h.mfaLockoutChecker.ResetMFAAttempts(r.Context(), userID); cerr != nil {
		slog.WarnContext(logCtx, "failed to clear mfa failures", "err", cerr, "user_id", userID)
	}

	// The challenge is spent before anything is signed, by a write only one
	// request wins. A completion that loses it, or cannot write it, is refused.
	if claims.ID != "" {
		won, merr := h.mfaLockoutChecker.MarkJTIUsed(r.Context(), claims.ID)
		if merr != nil {
			slog.WarnContext(logCtx, "mfa challenge spend failed, failing closed", "err", merr, "user_id", userID)
			respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeSessionStoreUnavailable, nil)
			return
		}
		if !won {
			respondErrCode(w, r, http.StatusUnauthorized, i18n.CodeChallengeExpired, nil)
			return
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
			liveEmail = u.SignInEmail()
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

	attempt.issued, attempt.tenant = true, liveTenantID
	h.setSessionCookie(w, token)
	h.setCSRFCookie(w, csrfToken)

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
