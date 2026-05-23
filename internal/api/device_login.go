package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// Device sign-in follows the shape of the OAuth 2.0 device authorization
// grant: a device asks for a pair of codes, a person approves the short one in
// the admin while signed in, and the device polls with the long one until it
// is answered with a session. It is not an OAuth endpoint and claims no
// compliance with one.
const (
	// deviceLoginTTL is how long a request waits for a decision.
	deviceLoginTTL = 10 * time.Minute
	// deviceLoginInterval is the polling interval a request starts with.
	deviceLoginInterval = 5
	// deviceLoginSlowDownStep is what a poll that comes too soon adds to the
	// interval, and deviceLoginMaxInterval is where the growth stops.
	deviceLoginSlowDownStep = 5
	deviceLoginMaxInterval  = 60
	// deviceLoginPollSlack forgives a poll that arrives this much early, so
	// network jitter on a device that waits the interval exactly is not read
	// as polling too fast.
	deviceLoginPollSlack = 500 * time.Millisecond
	// deviceLoginPendingPerAddress bounds the requests one address, or one
	// IPv6 /64, can hold open, and deviceLoginPendingMax the install's total,
	// so anonymous callers cannot fill the table from many addresses either.
	deviceLoginPendingPerAddress = 10
	deviceLoginPendingMax        = 1000
	// deviceLoginPruneEvery is how often expired rows are cleared in the
	// background, so an idle install does not keep them.
	deviceLoginPruneEvery = 10 * time.Minute
	// Expired rows stay an hour, so a device polling late still hears its
	// request expired, and are pruned a batch at a time on each new request.
	deviceLoginRetention  = time.Hour
	deviceLoginPruneBatch = 100
	// deviceLoginMaxBody caps the two public bodies, which carry one short
	// field each.
	deviceLoginMaxBody = 4 << 10
	// deviceCodeBytes is the device code's entropy.
	deviceCodeBytes = 32
	// deviceLoginClientNameMax is the longest client name, in characters.
	deviceLoginClientNameMax = 64
	// deviceLoginVerificationPath is the console page a person approves on.
	deviceLoginVerificationPath = "/admin/device"
)

// userCodeAlphabet leaves out 0, O, 1, I and L, which read alike.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// userCodeLength is the user code's length without its dash: 31^8 codes.
const userCodeLength = 8

// Error codes the token endpoint answers with, in the grant's vocabulary.
const (
	deviceErrPending      = "authorization_pending"
	deviceErrSlowDown     = "slow_down"
	deviceErrDenied       = "access_denied"
	deviceErrExpired      = "expired_token"
	deviceErrInvalidGrant = "invalid_grant"
	deviceErrInvalidReq   = "invalid_request"
)

type deviceLoginHandler struct {
	store *db.DeviceLoginStore
	users userStore
	auth  *AuthHandler
	host  core.Host
	// consoleURL is LYEVE_CONSOLE_URL, the console's public URL, which the
	// engine validated at boot. The page a person approves on is the
	// console's, so the verification URI is built on it and never on
	// LYEVE_BASE_URL, which names the engine.
	consoleURL string
	// production refuses a request when consoleURL is unset, because a
	// verification URI built on a guess sends the person to a dead page.
	production bool
	// multiTenant decides whether the implicit tenant a single-tenant request
	// resolves to is a choice the approver made or the absence of one.
	multiTenant bool
	now         func() time.Time
}

// deviceLoginStarted is the body of a new request.
type deviceLoginStarted struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// Start opens a sign-in request for a device.
// POST /api/admin/auth/device
// Body: {"client_name": "cli on build-box"}
// Auth: public, rate limited per address.
func (h *deviceLoginHandler) Start(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientName string `json:"client_name"`
	}
	if err := decodeDeviceBody(w, r, &body); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "The body must be a JSON object with client_name.")
		return
	}
	name, msg := normalizeClientName(body.ClientName)
	if msg != "" {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, msg)
		return
	}

	ctx := r.Context()
	uri, err := h.verificationURI()
	if err != nil {
		slog.WarnContext(ctx, "device login refused: the console URL is unset", "err", err)
		httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "Device sign-in is not configured on this install.")
		return
	}
	now := h.now().UTC().Truncate(time.Microsecond)
	if _, err := h.store.Prune(ctx, now.Add(-deviceLoginRetention), deviceLoginPruneBatch); err != nil {
		slog.WarnContext(ctx, "device login prune failed", "err", err)
	}

	ip := clientIP(r)
	net := requesterNet(ip)
	open, err := h.store.CountPendingFrom(ctx, net, now)
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not start the sign-in.")
		return
	}
	if open >= deviceLoginPendingPerAddress {
		httpx.ErrorReq(w, r, http.StatusTooManyRequests, "Too many sign-ins from this address are waiting for approval. Approve, deny or let them expire first.")
		return
	}
	all, err := h.store.CountPending(ctx, now)
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not start the sign-in.")
		return
	}
	if all >= deviceLoginPendingMax {
		slog.WarnContext(ctx, "device login refused: the install holds its limit of open requests", "open", all, "limit", deviceLoginPendingMax)
		w.Header().Set("Retry-After", "60")
		httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "Too many sign-ins are waiting for approval. Try again in a few minutes.")
		return
	}

	// A taken code is a unique violation. Three draws in a row colliding
	// means something other than chance, and the caller hears an outage.
	var started deviceLoginStarted
	for attempt := 0; ; attempt++ {
		deviceCode, userCode, err := newDeviceCodes()
		if err != nil {
			httpx.ErrorReq(w, r, http.StatusInternalServerError, "Could not start the sign-in.")
			return
		}
		row := &domain.DeviceLogin{
			ID:             uuid.New(),
			DeviceCodeHash: hashDeviceCode(deviceCode),
			UserCode:       userCode,
			ClientName:     name,
			RequesterIP:    ip,
			RequesterNet:   net,
			PollInterval:   deviceLoginInterval,
			CreatedAt:      now,
			ExpiresAt:      now.Add(deviceLoginTTL),
		}
		err = h.store.Create(ctx, row)
		if err == nil {
			display := displayUserCode(userCode)
			started = deviceLoginStarted{
				DeviceCode:              deviceCode,
				UserCode:                display,
				VerificationURI:         uri,
				VerificationURIComplete: uri + "?code=" + display,
				ExpiresIn:               int(deviceLoginTTL / time.Second),
				Interval:                deviceLoginInterval,
			}
			break
		}
		if httpx.StoreStatusFor(err) != http.StatusConflict || attempt == 2 {
			slog.WarnContext(ctx, "device login create failed", "err", err, "attempt", attempt+1)
			httpx.ErrorReq(w, r, http.StatusServiceUnavailable, "Could not start the sign-in.")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, started)
}

// verificationURI is the console page a person approves on. Unset, the console
// URL is the console dev server outside production and an error in
// production, the same rule core.ConsoleURL gives the plugins.
func (h *deviceLoginHandler) verificationURI() (string, error) {
	base := strings.TrimRight(strings.TrimSpace(h.consoleURL), "/")
	if base == "" {
		if h.production {
			return "", core.ErrConsoleURLUnset
		}
		base = core.DevConsoleURL
	}
	return base + deviceLoginVerificationPath, nil
}

// Lookup shows a signed-in admin what they are about to approve.
// GET /api/admin/auth/device/{user_code}
// Auth: a signed-in session, admin or super_admin.
//
// Besides the request it answers what approving would hand over: the tenant,
// the roles the session would carry and how long it would last, and the
// approver's own address beside the requester's. The client name is whatever
// the device sent. Only the address is the engine's own observation.
func (h *deviceLoginHandler) Lookup(w http.ResponseWriter, r *http.Request) {
	caller := deviceSessionCaller(w, r)
	if caller == nil {
		return
	}
	d, ok := h.decidable(w, r)
	if !ok {
		return
	}
	user, ok := h.callerAccount(w, r, caller)
	if !ok {
		return
	}
	bound := h.boundTenant(r, caller)
	_, roles, err := h.auth.resolveActingTenant(r.Context(), user, bound)
	if err != nil {
		if errors.Is(err, errNotAMember) {
			httpx.ErrorReq(w, r, http.StatusForbidden, "Your account is not a member of the tenant this session acts in.")
			return
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not read the sign-in.")
		return
	}
	approver := clientIP(r)
	httpx.JSON(w, http.StatusOK, map[string]any{
		"user_code":          displayUserCode(d.UserCode),
		"client_name":        d.ClientName,
		"requester_ip":       d.RequesterIP,
		"created_at":         d.CreatedAt,
		"expires_at":         d.ExpiresAt,
		"tenant_id":          core.TenantIDFromCtx(r.Context()),
		"roles":              roles,
		"session_expires_in": h.auth.expirySecs,
		"approver_ip":        approver,
		"same_address":       approver == d.RequesterIP,
	})
}

// callerAccount reads the signed-in account behind the session.
func (h *deviceLoginHandler) callerAccount(w http.ResponseWriter, r *http.Request, caller *auth.Claims) (*domain.User, bool) {
	userID, err := uuid.Parse(caller.UserID)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
		return nil, false
	}
	user, err := h.users.GetByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
			return nil, false
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not read your account.")
		return nil, false
	}
	return user, true
}

// boundTenant is the tenant an approval binds: the one the session acts in.
// On a single-tenant install a session with no tenant resolves to the
// implicit one without anybody choosing it, and binding that name would sign
// the device into a tenant the account is not a member of. The session's own
// empty tenant is bound instead, which resolves the same way.
func (h *deviceLoginHandler) boundTenant(r *http.Request, caller *auth.Claims) string {
	t := core.TenantIDFromCtx(r.Context())
	if !h.multiTenant && caller.TenantID == "" && t == apimw.ImplicitTenant {
		return ""
	}
	return t
}

// Approve binds the request to the caller and the tenant their session acts
// in. The device's next poll is answered with a session.
// POST /api/admin/auth/device/{user_code}/approve
// Auth: a signed-in session, admin or super_admin.
//
// Approving hands a device a bearer session, a credential the browser's own
// session never exposes, so the person confirms it is them as they do before
// issuing an admin token: with a current MFA code when the account has MFA
// enrolled and with the password otherwise, both counted against the
// account's lockouts. Without it, a script running in the admin, or a
// session token taken from elsewhere, could approve a device of its own and
// renew itself indefinitely.
// Body: {"password": "..."} or {"mfa_code": "123456"}.
func (h *deviceLoginHandler) Approve(w http.ResponseWriter, r *http.Request) {
	caller := deviceSessionCaller(w, r)
	if caller == nil {
		return
	}
	var body struct {
		Password string `json:"password"`
		MFACode  string `json:"mfa_code"`
	}
	if err := decodeDeviceBody(w, r, &body); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "The body must be a JSON object with password or mfa_code.")
		return
	}
	d, ok := h.decidable(w, r)
	if !ok {
		return
	}
	user, ok := h.callerAccount(w, r, caller)
	if !ok {
		return
	}
	if status, msg := h.auth.stepUp(r, user, body.MFACode, body.Password); status != 0 {
		httpx.ErrorReq(w, r, status, msg)
		return
	}
	ctx := r.Context()
	// The version the account is at now, which the session this request
	// arrived on has just been checked against. An exchange after the account's
	// sessions end, by a logout, a password change or an erasure, is refused.
	version, err := h.users.GetTokenVersion(ctx, user.ID)
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not approve the sign-in.")
		return
	}
	var tenant *string
	if t := h.boundTenant(r, caller); t != "" {
		tenant = &t
	}
	if err := h.store.Approve(ctx, d.ID, user.ID, tenant, version, h.now()); err != nil {
		h.decisionFailed(w, r, err, "Could not approve the sign-in.")
		return
	}
	h.audit(ctx, "auth.device_login.approve", d, clientIP(r), r.UserAgent())
	httpx.JSON(w, http.StatusOK, map[string]any{"status": domain.DeviceLoginApproved})
}

// Deny refuses the request. The device's next poll is told so.
// POST /api/admin/auth/device/{user_code}/deny
// Auth: a signed-in session, admin or super_admin.
func (h *deviceLoginHandler) Deny(w http.ResponseWriter, r *http.Request) {
	if deviceSessionCaller(w, r) == nil {
		return
	}
	d, ok := h.decidable(w, r)
	if !ok {
		return
	}
	if err := h.store.Deny(r.Context(), d.ID, h.now()); err != nil {
		h.decisionFailed(w, r, err, "Could not deny the sign-in.")
		return
	}
	h.audit(r.Context(), "auth.device_login.deny", d, clientIP(r), r.UserAgent())
	httpx.JSON(w, http.StatusOK, map[string]any{"status": domain.DeviceLoginDenied})
}

// decisionFailed answers a decision the store refused: someone decided the
// request, or it expired, between the read and the write.
func (h *deviceLoginHandler) decisionFailed(w http.ResponseWriter, r *http.Request, err error, msg string) {
	if errors.Is(err, domain.ErrConflict) {
		deviceNotDecidable(w, r, http.StatusConflict, "decided", "This sign-in was already approved or denied.")
		return
	}
	httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), msg)
}

// decidable reads the request the path's user code names and requires it to
// be pending and unexpired. A lookup answers 404 for every other state. A
// decision answers 409 for one already made, so a second approval says what
// happened rather than that the code is unknown.
func (h *deviceLoginHandler) decidable(w http.ResponseWriter, r *http.Request) (*domain.DeviceLogin, bool) {
	code, ok := normalizeUserCode(chi.URLParam(r, "user_code"))
	if !ok {
		deviceNotDecidable(w, r, http.StatusNotFound, "unknown", "No sign-in is waiting for this code.")
		return nil, false
	}
	d, err := h.store.GetByUserCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			deviceNotDecidable(w, r, http.StatusNotFound, "unknown", "No sign-in is waiting for this code.")
			return nil, false
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not read the sign-in.")
		return nil, false
	}
	if d.Status != domain.DeviceLoginPending {
		status := http.StatusConflict
		if r.Method == http.MethodGet {
			status = http.StatusNotFound
		}
		deviceNotDecidable(w, r, status, "decided", "This sign-in was already approved or denied.")
		return nil, false
	}
	if !h.now().Before(d.ExpiresAt) {
		deviceNotDecidable(w, r, http.StatusNotFound, "expired", "This code has expired. Start the sign-in again on the device.")
		return nil, false
	}
	return d, true
}

// deviceNotDecidable writes a refusal that names why, so the admin page can
// say whether the code expired, was decided or was never issued.
func deviceNotDecidable(w http.ResponseWriter, r *http.Request, status int, reason, msg string) {
	code := "not_found"
	if status == http.StatusConflict {
		code = "conflict"
	}
	httpx.JSON(w, status, map[string]string{
		"error":      msg,
		"code":       code,
		"reason":     reason,
		"request_id": logging.RequestIDFromCtx(r.Context()),
	})
}

// Token answers a device's poll: pending, slow down, denied, expired, or a
// session once the request is approved.
// POST /api/admin/auth/device/token
// Body: {"device_code": "..."}
// Auth: public, rate limited per address. The device code is the credential.
func (h *deviceLoginHandler) Token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := decodeDeviceBody(w, r, &body); err != nil || body.DeviceCode == "" || len(body.DeviceCode) > 128 {
		deviceTokenError(w, deviceErrInvalidReq, "The body must be a JSON object with device_code.", 0)
		return
	}
	ctx := r.Context()
	d, err := h.store.GetByDeviceCodeHash(ctx, hashDeviceCode(body.DeviceCode))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			deviceTokenError(w, deviceErrInvalidGrant, "The device code is not recognized.", 0)
			return
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not read the sign-in.")
		return
	}

	now := h.now().UTC()
	switch {
	case d.Status == domain.DeviceLoginDenied:
		deviceTokenError(w, deviceErrDenied, "The sign-in was denied.", 0)
		return
	case d.Status == domain.DeviceLoginUsed || !now.Before(d.ExpiresAt):
		deviceTokenError(w, deviceErrExpired, "The sign-in expired or was already used. Start it again.", 0)
		return
	case d.Status == domain.DeviceLoginPending:
		h.pollPending(w, r, d, now)
		return
	case d.Status == domain.DeviceLoginApproved:
		h.exchange(w, r, d)
		return
	}
	deviceTokenError(w, deviceErrExpired, "The sign-in expired or was already used. Start it again.", 0)
}

// pollPending answers a poll on a request still waiting: authorization
// pending, or slow down when it came sooner than the interval allows, which
// also grows the interval.
func (h *deviceLoginHandler) pollPending(w http.ResponseWriter, r *http.Request, d *domain.DeviceLogin, now time.Time) {
	ctx := r.Context()
	interval := time.Duration(d.PollInterval) * time.Second
	polled, err := h.store.MarkPolled(ctx, d.ID, now, now.Add(-interval+deviceLoginPollSlack))
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not read the sign-in.")
		return
	}
	if polled {
		deviceTokenError(w, deviceErrPending, "Waiting for the sign-in to be approved in the admin.", 0)
		return
	}
	if err := h.store.SlowDown(ctx, d.ID, now, deviceLoginSlowDownStep, deviceLoginMaxInterval); err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not read the sign-in.")
		return
	}
	next := min(d.PollInterval+deviceLoginSlowDownStep, deviceLoginMaxInterval)
	deviceTokenError(w, deviceErrSlowDown, "Polling too fast. Wait the interval between polls.", next)
}

// exchange turns an approved request into a session for the approver, once.
// The account is read again first: one disabled, past its expiry, whose
// sessions ended since the approval, no longer a member of the bound tenant or
// no longer an admin there gets no session.
func (h *deviceLoginHandler) exchange(w http.ResponseWriter, r *http.Request, d *domain.DeviceLogin) {
	ctx := r.Context()
	if d.UserID == nil || d.TokenVersion == nil {
		deviceTokenError(w, deviceErrDenied, "The sign-in was not approved by an account that can use it.", 0)
		return
	}
	user, err := h.users.GetByID(ctx, *d.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			h.refuseExchange(w, r, d, "account_gone")
			return
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not complete the sign-in.")
		return
	}
	now := h.now()
	switch {
	case user.Disabled:
		h.refuseExchange(w, r, d, "account_disabled")
		return
	case user.ExpiresAt != nil && !now.Before(*user.ExpiresAt):
		h.refuseExchange(w, r, d, "account_expired")
		return
	case user.TokenVersion != *d.TokenVersion:
		h.refuseExchange(w, r, d, "sessions_ended")
		return
	}
	requested := ""
	if d.TenantID != nil {
		requested = *d.TenantID
	}
	tenant, roles, err := h.auth.resolveActingTenant(ctx, user, requested)
	if err != nil {
		if errors.Is(err, errNotAMember) {
			h.refuseExchange(w, r, d, "not_a_member")
			return
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not complete the sign-in.")
		return
	}
	if !hasRole(roles, "admin") && !hasRole(roles, "super_admin") {
		h.refuseExchange(w, r, d, "not_an_admin")
		return
	}

	// Used before signing, so two polls racing on one approval cannot both
	// leave with a session.
	if err := h.store.MarkUsed(ctx, d.ID); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			deviceTokenError(w, deviceErrExpired, "The sign-in expired or was already used. Start it again.", 0)
			return
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "Could not complete the sign-in.")
		return
	}

	token, err := auth.Sign(h.auth.jwtSecret, h.auth.expirySecs, user.ID, user.Email, roles, tenant, user.TokenVersion)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusInternalServerError, "Could not complete the sign-in.")
		return
	}
	actor := context.WithValue(core.WithTenantID(ctx, tenant), core.ClaimsKey,
		&core.AuthClaims{UserID: user.ID.String(), Email: user.Email, Roles: roles, TenantID: tenant})
	h.audit(actor, "auth.device_login.exchange", d, clientIP(r), r.UserAgent())
	// The session starts on a device and an address the account may never
	// have used, so the device risk history hears of it as it hears of a
	// password login.
	if ra := h.auth.deviceRiskAssessor; ra != nil {
		if err := ra.RecordLogin(ctx, user.ID, collectFingerprint(r), true); err != nil {
			slog.WarnContext(ctx, "failed to record device login", "err", err)
		}
	}

	expires := now.Add(time.Duration(h.auth.expirySecs) * time.Second).UTC()
	httpx.JSON(w, http.StatusOK, map[string]any{
		"user":       safeUser(user),
		"token":      token,
		"token_type": "Bearer",
		"expires_in": h.auth.expirySecs,
		"expires_at": expires.Format(time.RFC3339),
	})
}

// refuseExchange answers an approval the account can no longer use. The row
// is left approved, so it expires on its own. The device is told it was
// denied. The refusal is audited with its reason under the approver: an
// exchange refused because the account's sessions ended since the approval
// can be the account's owner cutting off someone else's device.
func (h *deviceLoginHandler) refuseExchange(w http.ResponseWriter, r *http.Request, d *domain.DeviceLogin, reason string) {
	ctx := r.Context()
	slog.InfoContext(ctx, "device login exchange refused", "device_login_id", d.ID, "reason", reason)
	actor := ctx
	if d.UserID != nil {
		tenant := ""
		if d.TenantID != nil {
			tenant = *d.TenantID
		}
		actor = context.WithValue(core.WithTenantID(ctx, tenant), core.ClaimsKey,
			&core.AuthClaims{UserID: d.UserID.String(), TenantID: tenant})
	}
	h.auditWith(actor, "auth.device_login.refused", d, clientIP(r), r.UserAgent(), map[string]any{"reason": reason})
	deviceTokenError(w, deviceErrDenied, "The sign-in was not approved by an account that can use it.", 0)
}

// deviceTokenError writes one of the token endpoint's refusals. A positive
// interval is the one the device must now wait.
func deviceTokenError(w http.ResponseWriter, code, description string, interval int) {
	out := map[string]any{"error": code, "error_description": description}
	if interval > 0 {
		out["interval"] = interval
	}
	httpx.JSON(w, http.StatusBadRequest, out)
}

// audit records a decision or an exchange. The row carries the address the
// request came from, alongside the actor's own.
func (h *deviceLoginHandler) audit(ctx context.Context, action string, d *domain.DeviceLogin, ip, userAgent string) {
	h.auditWith(ctx, action, d, ip, userAgent, nil)
}

func (h *deviceLoginHandler) auditWith(ctx context.Context, action string, d *domain.DeviceLogin, ip, userAgent string, extra map[string]any) {
	if h.host == nil {
		return
	}
	after := map[string]any{
		"client_name":  d.ClientName,
		"requester_ip": d.RequesterIP,
	}
	for k, v := range extra {
		after[k] = v
	}
	if t := core.TenantIDFromCtx(ctx); t != "" {
		after["tenant_id"] = t
	}
	compliance.RecordAuditWithState(ctx, h.host, action, "device_login", d.ID.String(), ip, userAgent, nil, after)
}

// deviceSessionCaller returns the signed-in session behind the request, or
// writes the refusal and returns nil. An API key or an admin token cannot
// approve a sign-in: either would let a credential mint a person's session.
func deviceSessionCaller(w http.ResponseWriter, r *http.Request) *auth.Claims {
	if ac := core.GetClaims(r.Context()); ac != nil && (ac.IsAPIKey || ac.AdminTokenID != "") {
		httpx.ErrorReq(w, r, http.StatusForbidden, "A device sign-in is approved from a signed-in session.")
		return nil
	}
	c := claimsFromCtx(r)
	if c == nil {
		httpx.ErrorReq(w, r, http.StatusForbidden, "A device sign-in is approved from a signed-in session.")
		return nil
	}
	return c
}

// decodeDeviceBody reads one of the small public bodies.
func decodeDeviceBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, deviceLoginMaxBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// normalizeClientName trims the name and requires 1 to 64 printable
// characters.
func normalizeClientName(in string) (string, string) {
	name := strings.TrimSpace(in)
	if name == "" {
		return "", "client_name is required."
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > deviceLoginClientNameMax {
		return "", "client_name must be at most 64 characters."
	}
	for _, c := range name {
		if !unicode.IsPrint(c) {
			return "", "client_name must be printable text."
		}
	}
	return name, ""
}

// newDeviceCodes draws a device code and a user code.
func newDeviceCodes() (string, string, error) {
	buf := make([]byte, deviceCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	max := big.NewInt(int64(len(userCodeAlphabet)))
	code := make([]byte, userCodeLength)
	for i := range code {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", "", err
		}
		code[i] = userCodeAlphabet[n.Int64()]
	}
	return base64.RawURLEncoding.EncodeToString(buf), string(code), nil
}

// hashDeviceCode is the form the device code is stored and looked up in.
func hashDeviceCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// displayUserCode writes a stored user code as XXXX-XXXX.
func displayUserCode(code string) string {
	if len(code) != userCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// normalizeUserCode reads a user code the way a person types it: any case,
// with or without the dash or spaces. It reports false for anything that
// cannot be a code, which then never reaches the database.
func normalizeUserCode(in string) (string, bool) {
	var b strings.Builder
	for _, c := range strings.ToUpper(in) {
		if c == '-' || c == ' ' {
			continue
		}
		if !strings.ContainsRune(userCodeAlphabet, c) {
			return "", false
		}
		b.WriteRune(c)
	}
	if b.Len() != userCodeLength {
		return "", false
	}
	return b.String(), true
}

// requesterNet is the key the open-request cap counts an address under: the
// address itself for IPv4, and its /64 for IPv6, where a single host is
// routinely handed the whole prefix and could otherwise open requests from
// as many addresses as it likes.
func requesterNet(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

// runPruner clears expired requests every deviceLoginPruneEvery until ctx
// ends. A new request prunes a batch as well. This keeps an install that
// stops receiving them from holding addresses and account ids indefinitely.
func (h *deviceLoginHandler) runPruner(ctx context.Context) {
	t := time.NewTicker(deviceLoginPruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.prune(ctx)
		}
	}
}

// prune deletes expired requests a batch at a time until a batch comes back
// short.
func (h *deviceLoginHandler) prune(ctx context.Context) {
	cutoff := h.now().Add(-deviceLoginRetention)
	for {
		n, err := h.store.Prune(ctx, cutoff, deviceLoginPruneBatch)
		if err != nil {
			if ctx.Err() == nil {
				slog.WarnContext(ctx, "device login prune failed", "err", err)
			}
			return
		}
		if n < deviceLoginPruneBatch {
			return
		}
	}
}
