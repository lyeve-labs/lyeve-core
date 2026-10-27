package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/internal/jsonpool"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/reqparse"
)

// UsersHandler manages CMS admin users (super_admin only).
type UsersHandler struct {
	users            *db.UserStore
	passwordHashAlgo string
	passwordPolicy   auth.PasswordPolicy
	// revokeRefresh ends the user's refresh-token families after a password
	// is set. It is nil when refresh tokens are not configured.
	revokeRefresh func(ctx context.Context, userID string) error
	// seats refuses the write that would add an admin seat past the optional
	// ceiling a licensing implementation may state. The routers always set
	// it. Nil checks nothing.
	seats core.AdminSeatGuard
	// ceilingURL is where the licensing implementation sends an operator to
	// lift a ceiling, put in the seat refusal. Nil offers no link.
	ceilingURL func(cap string) string
	// host is the engine host the audit writer stores each account change
	// through. Nil records nothing.
	host core.Host
}

// NewUsersHandler constructs a UsersHandler. passwordHashAlgo defaults to
// "bcrypt" when empty or omitted.
func NewUsersHandler(users *db.UserStore, passwordHashAlgo ...string) *UsersHandler {
	algo := "bcrypt"
	if len(passwordHashAlgo) > 0 && passwordHashAlgo[0] != "" {
		algo = passwordHashAlgo[0]
	}
	return &UsersHandler{
		users:            users,
		passwordHashAlgo: algo,
		passwordPolicy:   auth.DefaultPasswordPolicy(),
	}
}

// List returns a paginated list of users.
// GET /api/admin/users
// Auth: super_admin.
func (h *UsersHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, err := reqparse.QueryInt(r, "limit", 25)
	if err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}
	limit = clampLimit(limit, 1, 200)
	offset, err := reqparse.QueryOffset(r)
	if err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}

	users, err := h.users.List(r.Context(), limit, offset)
	if err != nil {
		respondStoreErrCode(w, r, http.StatusServiceUnavailable, err, i18n.CodeDatabaseError)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, safeUser(u))
	}
	respond(w, http.StatusOK, out)
}

// Create adds a new CMS user.
// POST /api/admin/users
// Body: {"email": "...", "password": "...", "roles": ["..."]}
// Auth: super_admin.
func (h *UsersHandler) Create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string   `json:"email"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
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
	if len(body.Roles) == 0 {
		body.Roles = []string{"editor"}
	}

	// The tenant the account belongs to. An install with tenancy on resolves
	// none for a super admin who names no tenant on an address that maps to
	// none, and an account stored with the empty tenant can sign in but is
	// refused everywhere after, so it is not created.
	if core.TenantIDFromCtx(r.Context()) == "" {
		httpx.ErrorReq(w, r, http.StatusBadRequest, "Name the tenant with the X-Tenant-ID header.")
		return
	}

	hash, err := auth.HashPassword(h.passwordHashAlgo, body.Password)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodePasswordHashFailed, nil)
		return
	}

	// Capture tenant context from the request (set by middleware.TenantHeader).
	tenantID := core.TenantIDFromCtx(r.Context())

	var user *domain.User
	seatErr, err := core.WriteWithAdminSeat(r.Context(), h.seats, uuid.Nil, body.Roles, func(ctx context.Context) error {
		var err error
		user, err = h.users.Create(ctx, body.Email, hash, body.Roles, tenantID)
		return err
	})
	if !admitAdminSeat(w, r, seatErr, h.ceilingURL) {
		return
	}
	if errors.Is(err, domain.ErrValidation) {
		respondErrCode(w, r, http.StatusUnprocessableEntity, i18n.CodeValidationEmail, map[string]any{"field": "email"})
		return
	}
	if err != nil {
		// Detect unique-constraint violations across all dialects.
		errMsg := strings.ToLower(err.Error())
		if errors.Is(err, domain.ErrConflict) ||
			strings.Contains(errMsg, "duplicate key") ||
			strings.Contains(errMsg, "unique constraint") ||
			strings.Contains(errMsg, "duplicate entry") ||
			strings.Contains(errMsg, "violates unique") {
			respondErrCode(w, r, http.StatusConflict, i18n.CodeUserCreateFailed, nil)
			return
		}
		respondStoreErrCode(w, r, httpx.StoreStatusFor(err), err, i18n.CodeUserCreateFailed)
		return
	}
	h.audit(r, "user.create", user.ID, user.TenantID, nil, map[string]any{"roles": user.Roles})
	respond(w, http.StatusCreated, safeUser(user))
}

// UpdateRoles changes a user's roles.
// PUT /api/admin/users/{id}/roles
// Body: {"roles": ["..."]}
// Auth: super_admin.
func (h *UsersHandler) UpdateRoles(w http.ResponseWriter, r *http.Request) {
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidUserID, nil)
		return
	}

	var body struct {
		Roles []string `json:"roles"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil || len(body.Roles) == 0 {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeRolesRequired, nil)
		return
	}

	// The seat guard counts the account's current roles, so an unknown id has
	// to answer 404 before the guard can call it a new seat and answer 402.
	before, err := h.users.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondStoreErrCode(w, r, httpx.StoreStatusFor(err), err, i18n.CodeDatabaseError)
		return
	}

	var user *domain.User
	seatErr, err := core.WriteWithAdminSeat(r.Context(), h.seats, id, body.Roles, func(ctx context.Context) error {
		var err error
		user, err = h.users.UpdateRoles(ctx, id, body.Roles)
		return err
	})
	if !admitAdminSeat(w, r, seatErr, h.ceilingURL) {
		return
	}
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondStoreErrCode(w, r, http.StatusServiceUnavailable, err, i18n.CodeDatabaseError)
		return
	}
	h.audit(r, "user.roles.update", user.ID, user.TenantID, map[string]any{"roles": before.Roles}, map[string]any{"roles": user.Roles})
	respond(w, http.StatusOK, safeUser(user))
}

// admitAdminSeat answers the guard's verdict: true to proceed, otherwise the
// response is written. A refusal is the cap_exceeded body, with the link
// upgrade names for the seat ceiling when upgrade is set, and a count that
// could not be read is the database outage it is.
func admitAdminSeat(w http.ResponseWriter, r *http.Request, err error, upgrade func(cap string) string) bool {
	if err == nil {
		return true
	}
	var capErr *core.AdminSeatCapError
	if errors.As(err, &capErr) {
		link := ""
		if upgrade != nil {
			link = upgrade(core.CapAdminSeats)
		}
		httpx.CapExceeded(w, core.CapAdminSeats, capErr.Limit, capErr.Current, link)
		return false
	}
	respondStoreErrCode(w, r, httpx.StoreStatusFor(err), err, i18n.CodeDatabaseError)
	return false
}

// UpdateState sets the account lockout fields on a user.
// PUT /api/admin/users/{id}/state
// Auth: super_admin.
//
// Body keys are optional and only what is present is written, so updating
// "disabled" alone cannot clear an expiry the caller never mentioned.
func (h *UsersHandler) UpdateState(w http.ResponseWriter, r *http.Request) {
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidUserID, nil)
		return
	}

	var raw map[string]json.RawMessage
	if err := jsonpool.DecodeJSON(r.Body, &raw); err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeValidationFailed, nil)
		return
	}

	var disabled *bool
	if v, ok := raw["disabled"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			respondErrCode(w, r, http.StatusBadRequest, i18n.CodeValidationFailed, nil)
			return
		}
		disabled = &b
	}

	var expiresAt **time.Time
	if v, ok := raw["expires_at"]; ok {
		var t *time.Time
		if err := json.Unmarshal(v, &t); err != nil {
			respondErrCode(w, r, http.StatusBadRequest, i18n.CodeValidationFailed, nil)
			return
		}
		expiresAt = &t
	}

	if disabled == nil && expiresAt == nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeValidationFailed, nil)
		return
	}

	// Locking yourself out leaves nobody able to undo it: the only route that
	// clears the flag is this one, and it needs a session this call would kill.
	claims := claimsFromCtx(r)
	if claims != nil && claims.UserID == id.String() && disabled != nil && *disabled {
		respondErrCode(w, r, http.StatusForbidden, i18n.CodeForbidden, nil)
		return
	}

	user, err := h.users.SetAccountState(r.Context(), id, disabled, expiresAt)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondStoreErrCode(w, r, http.StatusServiceUnavailable, err, i18n.CodeDatabaseError)
		return
	}
	h.audit(r, "user.state.update", user.ID, user.TenantID, nil, map[string]any{"disabled": user.Disabled, "expires_at": user.ExpiresAt})
	respond(w, http.StatusOK, safeUser(user))
}

// SetPassword replaces a user's password.
// PUT /api/admin/users/{id}/password
// Body: {"password": "..."}
// Auth: super_admin.
//
// This is how an account whose owner forgot the password is recovered: the
// engine offers no self-service reset, so the person with the super_admin
// role sets a new one and hands it over. The new password meets the same
// policy a new account's does, and every session the account held ends with
// the change, on this node and on every other, because the token version
// moves with the hash.
func (h *UsersHandler) SetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidUserID, nil)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := jsonpool.DecodeJSON(r.Body, &body); err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidBody, nil)
		return
	}
	if err := auth.ValidatePassword(h.passwordPolicy, body.Password); err != nil {
		code, params := auth.PasswordViolationCode(err, h.passwordPolicy)
		respondErrCode(w, r, http.StatusUnprocessableEntity, code, params)
		return
	}

	hash, err := auth.HashPassword(h.passwordHashAlgo, body.Password)
	if err != nil {
		respondErrCode(w, r, http.StatusInternalServerError, i18n.CodePasswordHashFailed, nil)
		return
	}

	user, err := h.users.SetPassword(r.Context(), id, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		if errors.Is(err, domain.ErrConflict) {
			httpx.ErrorReq(w, r, http.StatusConflict, "this account signs in through a trusted issuer and has no password")
			return
		}
		respondStoreErrCode(w, r, http.StatusServiceUnavailable, err, i18n.CodeDatabaseError)
		return
	}
	// The hash and the token version moved together above, so the change is
	// already in force: refresh refuses a family issued under the old version.
	// Revoking here only frees the families early.
	if h.revokeRefresh != nil {
		if err := h.revokeRefresh(r.Context(), id.String()); err != nil {
			// The account may be another tenant's, and the line is about it.
			slog.WarnContext(logging.ContextAbout(r.Context(), user.TenantID), "set password: refresh tokens not revoked", "user_id", id.String(), "error", err)
		}
	}
	h.audit(r, "user.password.set", user.ID, user.TenantID, nil, nil)
	respond(w, http.StatusOK, safeUser(user))
}

// Delete removes a user.
// DELETE /api/admin/users/{id}
// Auth: super_admin.
func (h *UsersHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := reqparse.ParseUUID(r, "id")
	if err != nil {
		respondErrCode(w, r, http.StatusBadRequest, i18n.CodeInvalidUserID, nil)
		return
	}

	// Prevent deleting yourself.
	claims := claimsFromCtx(r)
	if claims != nil && claims.UserID == id.String() {
		respondErrCode(w, r, http.StatusForbidden, i18n.CodeForbidden, nil)
		return
	}

	// The account is read first because the entry is filed under its tenant
	// and names the roles the deletion took away.
	user, err := h.users.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondStoreErrCode(w, r, httpx.StoreStatusFor(err), err, i18n.CodeDatabaseError)
		return
	}
	if err := h.users.Delete(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondStoreErrCode(w, r, http.StatusServiceUnavailable, err, i18n.CodeDatabaseError)
		return
	}
	h.audit(r, "user.delete", id, user.TenantID, map[string]any{"roles": user.Roles}, nil)
	w.WriteHeader(http.StatusNoContent)
}

// audit records an account change. The entry is filed under the tenant the
// account belongs to, which for a super admin can differ from the one the
// request acts in: filed there, it would show another tenant's account to
// that tenant's admins. An account with no tenant files under none. Neither
// state ever carries a password or its hash.
func (h *UsersHandler) audit(r *http.Request, action string, id uuid.UUID, tenantID string, before, after map[string]any) {
	if h.host == nil {
		return
	}
	var opts []compliance.AuditOption
	if tenantID == "" {
		opts = append(opts, compliance.WithoutTenant())
	}
	var b, a any
	if before != nil {
		b = before
	}
	if after != nil {
		a = after
	}
	compliance.RecordAuditWithState(core.WithTenantID(r.Context(), tenantID), h.host, action, "user", id.String(),
		clientIP(r), r.UserAgent(), b, a, opts...)
}
