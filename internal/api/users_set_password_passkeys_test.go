package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// passkeyRevokerFake stands in for the plugin that stores passkeys. It
// records every account it was asked about and answers count or err.
type passkeyRevokerFake struct {
	asked []uuid.UUID
	count int
	err   error
}

func (f *passkeyRevokerFake) RevokePasskeys(_ context.Context, userID uuid.UUID) (int, error) {
	f.asked = append(f.asked, userID)
	return f.count, f.err
}

// passkeyReset is an account with a known password and a users handler that
// removes its passkeys through revoker.
type passkeyReset struct {
	store   *db.UserStore
	user    *domain.User
	h       *UsersHandler
	revoker *passkeyRevokerFake
}

const passkeyResetOldPassword = "the-old-password-1"

func newPasskeyReset(t *testing.T, store *db.UserStore, hash string) *passkeyReset {
	t.Helper()
	if hash == "" {
		var err error
		hash, err = auth.HashPassword("bcrypt", passkeyResetOldPassword)
		require.NoError(t, err)
	}
	user, err := store.Create(context.Background(), "passkey-reset-"+uuid.NewString()[:8]+"@example.com", hash, []string{"editor"}, "")
	require.NoError(t, err)
	revoker := &passkeyRevokerFake{count: 2}
	h := NewUsersHandler(store)
	h.passkeys = revoker
	return &passkeyReset{store: store, user: user, h: h, revoker: revoker}
}

func (p *passkeyReset) set(t *testing.T, id uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+id.String()+"/password", strings.NewReader(body))
	r = chiCtx(r, map[string]string{"id": id.String()})
	rr := httptest.NewRecorder()
	p.h.SetPassword(rr, r)
	return rr
}

func passkeysRemoved(t *testing.T, rr *httptest.ResponseRecorder) float64 {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	n, ok := out["passkeys_removed"].(float64)
	require.True(t, ok, "the answer says how many passkeys were removed: %s", rr.Body.String())
	return n
}

// A passkey signs in on its own, so one added from a stolen session would
// outlast the new password. With no choice in the body, setting a password
// removes the account's passkeys and says how many.
func TestUsersHandler_SetPasswordRemovesPasskeysByDefault(t *testing.T) {
	eachAccountDialect(t, func(t *testing.T, store *db.UserStore) {
		p := newPasskeyReset(t, store, "")
		rr := p.set(t, p.user.ID, `{"password":"The-new-password-2"}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, []uuid.UUID{p.user.ID}, p.revoker.asked)
		assert.Equal(t, float64(2), passkeysRemoved(t, rr))

		rr = p.set(t, p.user.ID, `{"password":"The-third-password-3","remove_passkeys":true}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Len(t, p.revoker.asked, 2, "an explicit choice to remove removes")
	})
}

// The super admin may keep the passkeys, and then the plugin is never asked.
func TestUsersHandler_SetPasswordKeepsPasskeysWhenAsked(t *testing.T) {
	eachAccountDialect(t, func(t *testing.T, store *db.UserStore) {
		p := newPasskeyReset(t, store, "")
		rr := p.set(t, p.user.ID, `{"password":"The-new-password-2","remove_passkeys":false}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Empty(t, p.revoker.asked)
		assert.Equal(t, float64(0), passkeysRemoved(t, rr))

		again, err := store.GetByID(context.Background(), p.user.ID)
		require.NoError(t, err)
		require.NoError(t, auth.VerifyPassword("bcrypt", again.PasswordHash, "The-new-password-2"))
	})
}

// When the passkeys cannot be removed the password stays as it was and every
// session it opened stays open, so the super admin can try again and nothing
// is half done.
func TestUsersHandler_SetPasswordRemovalFailureKeepsThePassword(t *testing.T) {
	eachAccountDialect(t, func(t *testing.T, store *db.UserStore) {
		ctx := context.Background()
		p := newPasskeyReset(t, store, "")
		p.revoker.err = errors.New("passkey store unreachable")
		before, err := store.GetTokenVersion(ctx, p.user.ID)
		require.NoError(t, err)

		rr := p.set(t, p.user.ID, `{"password":"The-new-password-2"}`)
		require.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "PASSKEYS_NOT_REMOVED")
		assert.NotContains(t, rr.Body.String(), "unreachable", "the cause stays in the log")

		again, err := store.GetByID(ctx, p.user.ID)
		require.NoError(t, err)
		require.NoError(t, auth.VerifyPassword("bcrypt", again.PasswordHash, passkeyResetOldPassword))
		after, err := store.GetTokenVersion(ctx, p.user.ID)
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})
}

// A trusted issuer's account has no password here, so the route refuses it
// before the plugin is asked to remove anything.
func TestUsersHandler_SetPasswordIssuerAccountRemovesNothing(t *testing.T) {
	eachAccountDialect(t, func(t *testing.T, store *db.UserStore) {
		p := newPasskeyReset(t, store, auth.ExternalIssuerMarker)
		rr := p.set(t, p.user.ID, `{"password":"The-new-password-2"}`)
		require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
		assert.Empty(t, p.revoker.asked)
	})
}

// An account that does not exist answers 404, and no passkeys are touched.
func TestUsersHandler_SetPasswordMissingAccountRemovesNothing(t *testing.T) {
	eachAccountDialect(t, func(t *testing.T, store *db.UserStore) {
		p := newPasskeyReset(t, store, "")
		rr := p.set(t, uuid.New(), `{"password":"The-new-password-2"}`)
		require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
		assert.Empty(t, p.revoker.asked)
	})
}
