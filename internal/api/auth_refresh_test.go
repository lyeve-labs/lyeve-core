package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// failingUserStore answers GetByID with a database failure while fail is set,
// which is the refresh handler failing after it has rotated the token.
type failingUserStore struct {
	*fakeUserStore
	fail bool
}

func (s *failingUserStore) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	if s.fail {
		return nil, errors.New("connection reset by peer")
	}
	return s.fakeUserStore.GetByID(ctx, id)
}

type refreshFixture struct {
	users *failingUserStore
	store *auth.RefreshTokenStore
	token string
	h     *AuthHandler
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	uid := uuid.New()
	user := &domain.User{ID: uid, Email: "retry@test.com", Roles: []string{"editor"}}
	users := &failingUserStore{fakeUserStore: &fakeUserStore{byEmail: map[string]*domain.User{user.Email: user}}}
	store := auth.NewRefreshTokenStore(auth.NewMemoryBackend(), "cms-test")
	h := NewAuthHandler(users, nil, nil, "test-secret-at-least-32-bytes-long!!", 3600, false)
	h.WithRefreshTokenStore(store, 15*time.Minute)
	issued, err := store.IssueForSession(context.Background(), uid.String(), "", 0, 15*time.Minute)
	require.NoError(t, err)
	return &refreshFixture{users: users, store: store, token: issued.RefreshToken, h: h}
}

func (f *refreshFixture) refresh(t *testing.T, token string) (int, string, string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"refresh_token": token})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/refresh", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.h.Refresh(rr, req)
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	next, _ := resp["refresh_token"].(string)
	return rr.Code, rr.Body.String(), next
}

// A refresh that fails after rotating answers 503, and the client keeps the
// token it sent. Its retry renews the session instead of reading as theft.
func TestRefresh_RetryAfterFailedAnswerRenews(t *testing.T) {
	f := newRefreshFixture(t)

	f.users.fail = true
	code, body, _ := f.refresh(t, f.token)
	require.Equal(t, http.StatusServiceUnavailable, code, body)

	f.users.fail = false
	code, body, next := f.refresh(t, f.token)
	assert.Equal(t, http.StatusOK, code, "retry after a 503: %s", body)
	assert.NotEmpty(t, next, "the retry answered no refresh token")
}

// Two console replicas can each trade one token a moment apart. The second
// trade is answered with a retryable status, and the successor the first one
// received still renews.
func TestRefresh_SecondTradeOfOneTokenIsRetryable(t *testing.T) {
	f := newRefreshFixture(t)

	code, body, successor := f.refresh(t, f.token)
	require.Equal(t, http.StatusOK, code, body)
	require.NotEmpty(t, successor)

	code, body, _ = f.refresh(t, f.token)
	assert.Equal(t, http.StatusConflict, code, "second trade of one token: %s", body)
	assert.NotContains(t, body, "AUTH_TOKEN_REUSE")

	code, body, _ = f.refresh(t, successor)
	assert.Equal(t, http.StatusOK, code, "the first trade's successor: %s", body)
}
