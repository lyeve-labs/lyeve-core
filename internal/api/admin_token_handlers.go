package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

const (
	// adminTokenMaxLifetime is the longest an admin token may live, counted
	// from when it is issued or rotated.
	adminTokenMaxLifetime = 90 * 24 * time.Hour
	// adminTokenClockSkew is how far past the maximum an expiry may land
	// and still be accepted, so a client that computed "90 days from now" a
	// moment before the engine did is not refused for it.
	adminTokenClockSkew = time.Minute
	// adminTokenRotationOverlap is how long a rotated token keeps working
	// beside its successor, at most.
	adminTokenRotationOverlap = 7 * 24 * time.Hour
	// adminTokenSecretBytes is the random part of a token.
	adminTokenSecretBytes = 32
	// adminTokenDisplayChars is how much of the random part the list shows.
	adminTokenDisplayChars = 8
	// adminTokenNameMax bounds a token's name.
	adminTokenNameMax = 255
	// adminTokenMaxAllowedIPs bounds a token's address list.
	adminTokenMaxAllowedIPs = 100
)

// adminTokenHandler serves /api/admin/admin-tokens. Every route is session
// only: an API key or an admin token calling one is refused, so no machine
// credential can mint, rotate or revoke another.
type adminTokenHandler struct {
	store    core.AdminTokenStore
	users    adminTokenUsers
	auth     *AuthHandler
	members  core.MembershipReader
	registry *adminGrantRegistry
	host     core.Host
	validate core.TenantValidatorFunc
	now      func() time.Time
}

// adminTokenCreated is the body of a create or rotate: the token, once.
type adminTokenCreated struct {
	*core.AdminToken
	Token string `json:"token"`
}

type adminTokenGrant struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Routes      []adminGrantRoute `json:"routes"`
}

// sessionCaller returns the signed-in session behind the request, or writes
// the refusal and returns nil.
func (h *adminTokenHandler) sessionCaller(w http.ResponseWriter, r *http.Request) *auth.Claims {
	if ac := core.GetClaims(r.Context()); ac != nil && (ac.IsAPIKey || ac.AdminTokenID != "") {
		httpx.ErrorReq(w, r, http.StatusForbidden, "Admin tokens are managed from a signed-in session.")
		return nil
	}
	c := claimsFromCtx(r)
	if c == nil {
		httpx.ErrorReq(w, r, http.StatusForbidden, "Admin tokens are managed from a signed-in session.")
		return nil
	}
	return c
}

// tenantOf returns the tenant the request acts in, or writes the refusal.
func tenantOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := core.TenantIDFromCtx(r.Context())
	if t == "" {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "Name the tenant with the X-Tenant-ID header.")
		return "", false
	}
	return t, true
}

// Grants lists the catalog, each grant with the routes that declare it.
// GET /api/admin/admin-tokens/grants
func (h *adminTokenHandler) Grants(w http.ResponseWriter, r *http.Request) {
	if h.sessionCaller(w, r) == nil {
		return
	}
	out := []adminTokenGrant{}
	for _, g := range core.AdminGrants() {
		out = append(out, adminTokenGrant{Name: g.Name, Description: g.Description, Routes: h.registry.routesFor(g.Name)})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// List returns the tenant's tokens: a super admin sees every one, an admin
// only their own.
// GET /api/admin/admin-tokens?limit&offset
func (h *adminTokenHandler) List(w http.ResponseWriter, r *http.Request) {
	caller := h.sessionCaller(w, r)
	if caller == nil {
		return
	}
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	limit, offset, ok := pageParams(w, r)
	if !ok {
		return
	}
	var owner *uuid.UUID
	if !caller.HasRole("super_admin") {
		id, err := uuid.Parse(caller.UserID)
		if err != nil {
			httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
			return
		}
		owner = &id
	}
	tokens, total, err := h.store.List(r.Context(), tenant, owner, limit, offset)
	if err != nil {
		slog.WarnContext(r.Context(), "list admin tokens failed", "err", err)
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to list admin tokens")
		return
	}
	httpx.Paginated(w, tokens, total, limit, offset)
}

type createAdminTokenInput struct {
	Name       string     `json:"name"`
	Grants     []string   `json:"grants"`
	ExpiresAt  *time.Time `json:"expires_at"`
	AllowedIPs []string   `json:"allowed_ips"`
	TenantID   string     `json:"tenant_id"`
	MFACode    string     `json:"mfa_code"`
	Password   string     `json:"password"`
}

// Create issues a token owned by the caller. The token is in the response
// and nowhere else.
// POST /api/admin/admin-tokens
func (h *adminTokenHandler) Create(w http.ResponseWriter, r *http.Request) {
	caller := h.sessionCaller(w, r)
	if caller == nil {
		return
	}
	var in createAdminTokenInput
	if err := jsonpool.DecodeJSON(r.Body, &in); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	now := h.now()

	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > adminTokenNameMax {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, "name is required and must be at most 255 characters.")
		return
	}
	grants, msg := normalizeGrants(in.Grants)
	if msg != "" {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, msg) //nolint:raw-error-text // built from the caller's own grant names
		return
	}
	expires, msg := checkExpiry(in.ExpiresAt, now)
	if msg != "" {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, msg)
		return
	}
	ips, msg := normalizeAllowedIPs(in.AllowedIPs)
	if msg != "" {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, msg) //nolint:raw-error-text // names the caller's own entry
		return
	}

	tenant, status, msg := h.tokenTenant(r, caller, strings.TrimSpace(in.TenantID))
	if status != 0 {
		httpx.ErrorReq(w, r, status, msg)
		return
	}
	owner, ok := h.owner(w, r, caller, tenant)
	if !ok {
		return
	}
	if status, msg := h.auth.stepUp(r, owner, in.MFACode, in.Password); status != 0 {
		httpx.ErrorReq(w, r, status, msg)
		return
	}

	raw, tok, err := newAdminToken(owner, tenant, name, grants, ips, expires, now, nil)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusInternalServerError, "failed to create admin token")
		return
	}
	if err := h.store.Create(r.Context(), tok); err != nil {
		slog.WarnContext(r.Context(), "create admin token failed", "err", err)
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to create admin token")
		return
	}
	tok.OwnerEmail = owner.Email
	h.audit(r, "admin_token.create", tok)
	httpx.JSON(w, http.StatusCreated, adminTokenCreated{AdminToken: tok, Token: raw})
}

type rotateAdminTokenInput struct {
	ExpiresAt *time.Time `json:"expires_at"`
	MFACode   string     `json:"mfa_code"`
	Password  string     `json:"password"`
}

// Rotate issues a successor to one of the caller's tokens, with the same
// name, grants, tenant and address list and a new expiry. The old token
// keeps working for at most seven more days, so a deployment can switch
// over without a gap.
// POST /api/admin/admin-tokens/{id}/rotate
func (h *adminTokenHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	caller := h.sessionCaller(w, r)
	if caller == nil {
		return
	}
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusNotFound, "admin token not found")
		return
	}
	var in rotateAdminTokenInput
	if err := jsonpool.DecodeJSON(r.Body, &in); err != nil {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}
	now := h.now()
	expires, msg := checkExpiry(in.ExpiresAt, now)
	if msg != "" {
		httpx.ErrorReq(w, r, http.StatusUnprocessableEntity, msg)
		return
	}
	old, err := h.store.Get(r.Context(), tenant, id)
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "admin token not found")
		return
	}
	// Only the owner rotates: the successor is shown to whoever asks, and it
	// acts as the owner.
	if old.OwnerUserID.String() != caller.UserID {
		httpx.ErrorReq(w, r, http.StatusForbidden, "Only the owner of an admin token can rotate it.")
		return
	}
	if old.RevokedAt != nil || !now.Before(old.ExpiresAt) {
		httpx.ErrorReq(w, r, http.StatusConflict, "A revoked or expired admin token cannot be rotated.")
		return
	}
	owner, ok := h.owner(w, r, caller, tenant)
	if !ok {
		return
	}
	if status, msg := h.auth.stepUp(r, owner, in.MFACode, in.Password); status != 0 {
		httpx.ErrorReq(w, r, status, msg)
		return
	}

	raw, tok, err := newAdminToken(owner, tenant, old.Name, old.Grants, old.AllowedIPs, expires, now, &old.ID)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusInternalServerError, "failed to rotate admin token")
		return
	}
	if err := h.store.Rotate(r.Context(), tenant, old.ID, tok, now.Add(adminTokenRotationOverlap)); err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			slog.WarnContext(r.Context(), "rotate admin token failed", "err", err)
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to rotate admin token")
		return
	}
	tok.OwnerEmail = owner.Email
	h.audit(r, "admin_token.rotate", tok)
	httpx.JSON(w, http.StatusCreated, adminTokenCreated{AdminToken: tok, Token: raw})
}

// Revoke ends a token now. Its owner or a super admin of the tenant may.
// DELETE /api/admin/admin-tokens/{id}
func (h *adminTokenHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	caller := h.sessionCaller(w, r)
	if caller == nil {
		return
	}
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusNotFound, "admin token not found")
		return
	}
	tok, err := h.store.Get(r.Context(), tenant, id)
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "admin token not found")
		return
	}
	if tok.OwnerUserID.String() != caller.UserID && !caller.HasRole("super_admin") {
		httpx.ErrorReq(w, r, http.StatusForbidden, "Only the owner of an admin token or a super admin can revoke it.")
		return
	}
	if err := h.store.Revoke(r.Context(), tenant, id, h.now()); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			httpx.ErrorReq(w, r, http.StatusConflict, "This admin token is already revoked.")
			return
		}
		slog.WarnContext(r.Context(), "revoke admin token failed", "err", err)
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to revoke admin token")
		return
	}
	h.audit(r, "admin_token.revoke", tok)
	w.WriteHeader(http.StatusNoContent)
}

// Requests pages through one token's request log, newest first. Readable by
// whoever may see the token in the list.
// GET /api/admin/admin-tokens/{id}/requests?limit&offset
func (h *adminTokenHandler) Requests(w http.ResponseWriter, r *http.Request) {
	caller := h.sessionCaller(w, r)
	if caller == nil {
		return
	}
	tenant, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusNotFound, "admin token not found")
		return
	}
	limit, offset, ok := pageParams(w, r)
	if !ok {
		return
	}
	tok, err := h.store.Get(r.Context(), tenant, id)
	if err != nil {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "admin token not found")
		return
	}
	if tok.OwnerUserID.String() != caller.UserID && !caller.HasRole("super_admin") {
		httpx.ErrorReq(w, r, http.StatusNotFound, "admin token not found")
		return
	}
	rows, total, err := h.store.ListRequests(r.Context(), tenant, id, limit, offset)
	if err != nil {
		slog.WarnContext(r.Context(), "list admin token requests failed", "err", err)
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "failed to list admin token requests")
		return
	}
	httpx.Paginated(w, rows, total, limit, offset)
}

// tokenTenant decides the tenant a new token is bound to. A super admin may
// name any tenant the roster knows. Anyone else gets the tenant they act in.
func (h *adminTokenHandler) tokenTenant(r *http.Request, caller *auth.Claims, requested string) (string, int, string) {
	current := core.TenantIDFromCtx(r.Context())
	if requested == "" || requested == current {
		if current == "" {
			return "", http.StatusUnprocessableEntity, "tenant_id is required."
		}
		return current, 0, ""
	}
	if !caller.HasRole("super_admin") {
		return "", http.StatusForbidden, "An admin token can only be issued for the tenant you act in."
	}
	if !core.IsValidTenantSlug(requested) {
		return "", http.StatusUnprocessableEntity, "tenant_id is not a valid tenant."
	}
	if h.validate != nil && !h.validate(r.Context(), requested) {
		return "", http.StatusUnprocessableEntity, "tenant_id is not a valid tenant."
	}
	return requested, 0, ""
}

// owner reads the caller's account and checks it may own a token in tenant:
// it must hold admin or super_admin there.
func (h *adminTokenHandler) owner(w http.ResponseWriter, r *http.Request, caller *auth.Claims, tenant string) (*domain.User, bool) {
	uid, err := uuid.Parse(caller.UserID)
	if err != nil {
		httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
		return nil, false
	}
	u, err := h.users.GetByID(r.Context(), uid)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			httpx.ErrorReq(w, r, http.StatusUnauthorized, "session invalidated")
			return nil, false
		}
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "account check unavailable")
		return nil, false
	}
	roles, err := ownerRolesIn(r.Context(), h.members, u, tenant)
	if err != nil && !errors.Is(err, errNotAMember) {
		httpx.ErrorReq(w, r, httpx.StoreStatusFor(err), "account check unavailable")
		return nil, false
	}
	if !hasRole(roles, "admin") && !hasRole(roles, "super_admin") {
		httpx.ErrorReq(w, r, http.StatusForbidden, "An admin token's owner must be an admin of its tenant.")
		return nil, false
	}
	return u, true
}

func (h *adminTokenHandler) audit(r *http.Request, action string, tok *core.AdminToken) {
	if h.host == nil {
		return
	}
	compliance.RecordAudit(r.Context(), h.host, action, "admin_token", tok.ID.String(),
		clientIP(r), r.UserAgent())
}

// clientIP is the caller's address as the ClientAddress middleware resolved
// it onto the request.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// newAdminToken mints a token for owner and returns it with its row.
func newAdminToken(owner *domain.User, tenant, name string, grants, ips []string, expires, now time.Time, rotatedFrom *uuid.UUID) (string, *core.AdminToken, error) {
	buf := make([]byte, adminTokenSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	raw := core.AdminTokenPrefix + secret
	return raw, &core.AdminToken{
		ID:            uuid.New(),
		TenantID:      tenant,
		OwnerUserID:   owner.ID,
		Name:          name,
		TokenHash:     security.HashKeyPeppered(raw),
		DisplayPrefix: secret[:adminTokenDisplayChars],
		Grants:        append([]string{}, grants...),
		AllowedIPs:    append([]string{}, ips...),
		ExpiresAt:     expires.UTC().Truncate(time.Microsecond),
		CreatedAt:     now.UTC().Truncate(time.Microsecond),
		RotatedFrom:   rotatedFrom,
	}, nil
}

// normalizeGrants checks every grant against the catalog and drops repeats.
func normalizeGrants(in []string) ([]string, string) {
	if len(in) == 0 {
		return nil, "grants must name at least one grant."
	}
	seen := map[string]bool{}
	out := []string{}
	for _, g := range in {
		g = strings.TrimSpace(g)
		if !core.IsAdminGrant(g) {
			return nil, "grants names " + strconv.Quote(g) + ", which is not an admin grant."
		}
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out, ""
}

// checkExpiry requires an expiry in the future and at most 90 days away.
func checkExpiry(at *time.Time, now time.Time) (time.Time, string) {
	if at == nil || at.IsZero() {
		return time.Time{}, "expires_at is required."
	}
	if !at.After(now) {
		return time.Time{}, "expires_at must be in the future."
	}
	if at.After(now.Add(adminTokenMaxLifetime + adminTokenClockSkew)) {
		return time.Time{}, "expires_at must be at most 90 days away."
	}
	return at.UTC(), ""
}

// normalizeAllowedIPs reads each entry as a range, the way IP_ALLOWLIST is
// read, and refuses the list on the first entry that is neither an address
// nor a range.
func normalizeAllowedIPs(in []string) ([]string, string) {
	if len(in) > adminTokenMaxAllowedIPs {
		return nil, "allowed_ips holds more than 100 entries."
	}
	out := []string{}
	for _, e := range in {
		e = strings.TrimSpace(e)
		cidr, err := config.ParseIPEntry(e)
		if e == "" || strings.Contains(e, ",") || err != nil {
			return nil, "allowed_ips entry " + strconv.Quote(e) + " is not an IP address or CIDR range."
		}
		out = append(out, cidr)
	}
	return out, ""
}

// pageParams reads limit (1 to 100, default 50) and offset (default 0).
func pageParams(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	limit, offset := 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "limit must be between 1 and 100")
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			httpx.ErrorReq(w, r, http.StatusBadRequest, "offset must be zero or more")
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

// stepUp confirms, before a token is issued or rotated, that the person at
// the session is its owner. An account with MFA enrolled answers with a
// current code. One without answers with its password. A zero status passes.
//
// Both answers are guessable by a caller who already holds the session, so
// both are counted: a wrong code against the MFA lockout, a wrong password
// against the login lockout, and either locked answers 429. A code that was
// accepted once is not accepted again within its window.
func (h *AuthHandler) stepUp(r *http.Request, u *domain.User, mfaCode, password string) (int, string) {
	ctx := r.Context()
	enrolled := false
	if h.mfaStore != nil {
		on, err := h.mfaStore.IsEnabled(ctx, u.ID)
		if err != nil {
			slog.WarnContext(ctx, "step-up mfa check failed", "err", err)
			return http.StatusServiceUnavailable, "account check unavailable"
		}
		enrolled = on
	}
	if enrolled {
		return h.stepUpCode(r, u, strings.TrimSpace(mfaCode))
	}
	return h.stepUpPassword(r, u, password)
}

func (h *AuthHandler) stepUpCode(r *http.Request, u *domain.User, code string) (int, string) {
	ctx := r.Context()
	if code == "" {
		return http.StatusForbidden, "mfa_code is required: this account has MFA enrolled."
	}
	locked, err := h.mfaLockoutChecker.CheckMFALockout(ctx, u.ID)
	if err != nil {
		return http.StatusServiceUnavailable, "account check unavailable"
	}
	if locked {
		return http.StatusTooManyRequests, "Too many MFA attempts. Try again later."
	}
	encSecret, _, err := h.mfaStore.GetEnabled(ctx, u.ID)
	if err != nil {
		return http.StatusServiceUnavailable, "account check unavailable"
	}
	secret, err := h.totpSecretAny(stepUpSealTenants(r, u), encSecret)
	if err != nil {
		slog.WarnContext(ctx, "step-up totp secret unreadable", "err", err, "user_id", u.ID)
		return http.StatusInternalServerError, "account check unavailable"
	}
	outcome, err := acceptTOTP(ctx, h.mfaStore, u.ID, secret, code)
	if err != nil {
		slog.WarnContext(ctx, "step-up mfa code check failed", "err", err, "user_id", u.ID)
		return http.StatusServiceUnavailable, "account check unavailable"
	}
	switch outcome {
	case totpReplayed:
		return http.StatusForbidden, "This MFA code was already used. Wait for the next one."
	case totpInvalid:
		if rerr := h.mfaLockoutChecker.RecordMFAFailure(ctx, u.ID); rerr != nil {
			slog.WarnContext(ctx, "failed to record mfa failure", "err", rerr)
		}
		return http.StatusForbidden, "The MFA code is not valid."
	}
	return 0, ""
}

func (h *AuthHandler) stepUpPassword(r *http.Request, u *domain.User, password string) (int, string) {
	ctx := r.Context()
	if password == "" {
		return http.StatusForbidden, "password is required to confirm this change."
	}
	if h.lockoutChecker != nil {
		count, err := h.lockoutChecker.CountRecentFailures(ctx, u.ID, time.Now().Add(-h.LockoutWindow))
		if err != nil {
			slog.WarnContext(ctx, "step-up lockout check failed, failing closed", "err", err, "user_id", u.ID)
			return http.StatusServiceUnavailable, "account check unavailable"
		}
		if count >= h.MaxFailedAttempts {
			slog.WarnContext(ctx, "step-up refused: account is locked out", "user_id", u.ID,
				"failures", count, "max_attempts", h.MaxFailedAttempts, "window", h.LockoutWindow)
			return http.StatusTooManyRequests, "Too many failed password attempts. Try again later."
		}
	}
	if err := auth.VerifyPassword(h.passwordHashAlgo, u.PasswordHash, password); err != nil {
		if h.lockoutChecker != nil {
			if lerr := h.lockoutChecker.RecordFailedAttempt(ctx, u.ID); lerr != nil {
				slog.WarnContext(ctx, "failed to record lockout attempt", "err", lerr)
			}
		}
		return http.StatusForbidden, "The password is not valid."
	}
	return 0, ""
}

// stepUpSealTenants lists the tenants the owner's TOTP secret may have been
// sealed under, most likely first: the session's tenant claim, the tenant the
// request resolved to, the account's home tenant and the default key id. A
// super admin with no home tenant who enrolled while acting in another tenant
// has a secret sealed under that tenant.
func stepUpSealTenants(r *http.Request, u *domain.User) []string {
	var out []string
	add := func(t string) {
		for _, have := range out {
			if have == t {
				return
			}
		}
		out = append(out, t)
	}
	if c := claimsFromCtx(r); c != nil && c.TenantID != "" {
		add(c.TenantID)
	}
	if t := core.TenantIDFromCtx(r.Context()); t != "" {
		add(t)
	}
	if u.TenantID != "" {
		add(u.TenantID)
	}
	add(mfaDefaultTenantKeyID)
	return out
}
