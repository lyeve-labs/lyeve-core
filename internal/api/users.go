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
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
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
		code, params := passwordViolationResponse(err, h.passwordPolicy)
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
	if !admitAdminSeat(w, r, seatErr) {
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
		respondErrCode(w, r, httpx.StoreStatusFor(err), i18n.CodeUserCreateFailed, nil)
		return
	}
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
	if _, err := h.users.GetByID(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondErrCode(w, r, httpx.StoreStatusFor(err), i18n.CodeDatabaseError, nil)
		return
	}

	var user *domain.User
	seatErr, err := core.WriteWithAdminSeat(r.Context(), h.seats, id, body.Roles, func(ctx context.Context) error {
		var err error
		user, err = h.users.UpdateRoles(ctx, id, body.Roles)
		return err
	})
	if !admitAdminSeat(w, r, seatErr) {
		return
	}
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}
	respond(w, http.StatusOK, safeUser(user))
}

// admitAdminSeat answers the guard's verdict: true to proceed, otherwise the
// response is written. A refusal is the cap_exceeded body, and a count that
// could not be read is the database outage it is.
func admitAdminSeat(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return true
	}
	var capErr *core.AdminSeatCapError
	if errors.As(err, &capErr) {
		httpx.CapExceeded(w, core.CapAdminSeats, capErr.Limit, capErr.Current, "")
		return false
	}
	respondErrCode(w, r, httpx.StoreStatusFor(err), i18n.CodeDatabaseError, nil)
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
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}
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
		code, params := passwordViolationResponse(err, h.passwordPolicy)
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
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}
	// The hash and the token version moved together above, so the change is
	// already in force: refresh refuses a family issued under the old version.
	// Revoking here only frees the families early.
	if h.revokeRefresh != nil {
		if err := h.revokeRefresh(r.Context(), id.String()); err != nil {
			slog.WarnContext(r.Context(), "set password: refresh tokens not revoked", "user_id", id.String(), "error", err)
		}
	}
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

	if err := h.users.Delete(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			respondErrCode(w, r, http.StatusNotFound, i18n.CodeUserNotFound, nil)
			return
		}
		respondErrCode(w, r, http.StatusServiceUnavailable, i18n.CodeDatabaseError, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
