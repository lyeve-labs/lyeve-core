//go:build !mutest

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
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// The engine offers no self-service password reset, so this route is the
// only way an existing account's password changes. It has to hold the
// account's policy, end the sessions the old password opened, and answer a
// missing account plainly.
func TestUsersHandler_SetPassword(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.Postgres(t)
	store := db.NewUserStore(pool)
	ctx := context.Background()

	oldHash, err := auth.HashPassword("bcrypt", "the-old-password-1")
	require.NoError(t, err)
	u, err := store.Create(ctx, "set-password@ex.com", oldHash, []string{"editor"}, "")
	require.NoError(t, err)
	defer func() { _ = store.Delete(ctx, u.ID) }()

	h := NewUsersHandler(store)
	call := func(id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/admin/users/"+id+"/password", strings.NewReader(body))
		r = chiCtx(r, map[string]string{"id": id})
		rr := httptest.NewRecorder()
		h.SetPassword(rr, r)
		return rr
	}

	t.Run("policy applies", func(t *testing.T) {
		rr := call(u.ID.String(), `{"password":"short"}`)
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		again, err := store.GetByID(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, oldHash, again.PasswordHash, "a refused password must leave the hash alone")
	})

	t.Run("bad body", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, call(u.ID.String(), "{").Code)
		require.Equal(t, http.StatusBadRequest, call("not-a-uuid", `{"password":"A-long-enough-one-1"}`).Code)
	})

	t.Run("unknown user", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, call(uuid.NewString(), `{"password":"A-long-enough-one-1"}`).Code)
	})

	t.Run("sets the hash and ends every session", func(t *testing.T) {
		before, err := store.GetTokenVersion(ctx, u.ID)
		require.NoError(t, err)
		revoked := ""
		h.revokeRefresh = func(_ context.Context, userID string) error { revoked = userID; return nil }

		rr := call(u.ID.String(), `{"password":"The-new-password-2"}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		_, leaked := out["password_hash"]
		require.False(t, leaked, "the response must not carry the hash")

		again, err := store.GetByID(ctx, u.ID)
		require.NoError(t, err)
		require.NotEqual(t, oldHash, again.PasswordHash)
		require.NoError(t, auth.VerifyPassword("bcrypt", again.PasswordHash, "The-new-password-2"))
		after, err := store.GetTokenVersion(ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, before+1, after, "sessions opened by the old password must end with it")
		require.Equal(t, u.ID.String(), revoked, "refresh families are revoked too")
	})

	t.Run("a revoker failure does not undo the change", func(t *testing.T) {
		h.revokeRefresh = func(context.Context, string) error { return errors.New("redis away") }
		rr := call(u.ID.String(), `{"password":"The-third-password-3"}`)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		again, err := store.GetByID(ctx, u.ID)
		require.NoError(t, err)
		require.NoError(t, auth.VerifyPassword("bcrypt", again.PasswordHash, "The-third-password-3"))
	})
}
