package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// Logout revokes the refresh family only when the caller presents a token of
// that family that the store holds and the family is the caller's own. A
// family id is enough to name a family, so revoking on the id alone let any
// signed-in user end another person's session.
func TestLogout_RevokesOnlyTheCallersOwnFamily(t *testing.T) {
	victim := newRefreshFixture(t)
	familyID, _, err := auth.RefreshTokenFamilyID(victim.token)
	require.NoError(t, err)

	otherOwner := uuid.NewString()
	other, err := victim.store.IssueForSession(context.Background(), otherOwner, "", 0, 15*time.Minute, time.Time{})
	require.NoError(t, err)

	logout := func(userID, refreshToken string) {
		t.Helper()
		body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		claims := makeClaims(uuid.MustParse(userID), "caller@test.com", []string{"editor"})
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		rr := httptest.NewRecorder()
		victim.h.Logout(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	}
	ownerOf := func(token string) string {
		t.Helper()
		res, err := victim.store.Rotate(context.Background(), token, 15*time.Minute)
		require.NoError(t, err, "the family must still be live")
		return res.UserID
	}

	// Another user names the victim's family with a token the store never issued.
	logout(uuid.NewString(), familyID+".forged")
	victimUser := ownerOf(victim.token)

	// Another user presents a real token of a family that is not theirs.
	stranger, err := victim.store.IssueForSession(context.Background(), victimUser, "", 0, 15*time.Minute, time.Time{})
	require.NoError(t, err)
	logout(uuid.NewString(), stranger.RefreshToken)
	assert.Equal(t, victimUser, ownerOf(stranger.RefreshToken))

	// The owner presenting their own token ends that family.
	logout(otherOwner, other.RefreshToken)
	_, err = victim.store.Rotate(context.Background(), other.RefreshToken, 15*time.Minute)
	assert.Error(t, err, "the family of a token whose owner logged out must be revoked")
}
