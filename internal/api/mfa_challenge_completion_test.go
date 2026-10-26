package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// challengeCompletion is an account at home in acme that is also an admin of
// globex, and a handler that knows both.
type challengeCompletion struct {
	h    *AuthHandler
	user *domain.User
}

const challengeCompletionSecret = "test-secret-at-least-32-bytes-long!!"

func newChallengeCompletion(t *testing.T) *challengeCompletion {
	t.Helper()
	user := &domain.User{ID: uuid.New(), Email: "member@example.com", TenantID: "acme", Roles: []string{"editor"}, TokenVersion: 3}
	h := NewAuthHandler(&fakeUserStore{byEmail: map[string]*domain.User{user.Email: user}}, &fakeMFAStore{}, nil, challengeCompletionSecret, 3600, false)
	members := newFakeMemberships()
	_, err := members.Grant(context.Background(), user.ID, "globex", []string{"admin"})
	require.NoError(t, err)
	h.WithMemberships(members)
	return &challengeCompletion{h: h, user: user}
}

// onGlobexDomain is a request context on globex's mapped domain, as the host
// resolver leaves it.
func onGlobexDomain() context.Context {
	return core.WithHostTenantID(core.WithTenantID(context.Background(), "globex"), "globex")
}

func (c *challengeCompletion) challenge(t *testing.T, tenant string, roles []string) string {
	t.Helper()
	token, err := auth.SignChallenge(challengeCompletionSecret, c.user.ID, c.user.Email, roles, tenant)
	require.NoError(t, err)
	return token
}

// A password sign-in on globex's domain challenges for globex. A factor the
// plugin verified completes it as the code completion does: a session acting
// in globex with the membership's roles and the account's token version.
func TestCompleteMFAChallenge_SignsForTheChallengesTenant(t *testing.T) {
	t.Parallel()
	c := newChallengeCompletion(t)
	challenge := c.challenge(t, "globex", []string{"admin"})
	ctx := onGlobexDomain()

	account, err := c.h.MFAChallengeAccount(ctx, challenge)
	require.NoError(t, err)
	assert.Equal(t, core.MFAChallengeAccount{UserID: c.user.ID, Email: c.user.Email, TenantID: "globex"}, account)

	token, err := c.h.CompleteMFAChallenge(ctx, challenge)
	require.NoError(t, err)
	claims, err := auth.Parse(challengeCompletionSecret, token)
	require.NoError(t, err)
	assert.Equal(t, "globex", claims.TenantID)
	assert.Equal(t, []string{"admin"}, claims.Roles)
	assert.Equal(t, 3, claims.TokenVersion)
	assert.False(t, claims.MFAPending)
}

// One challenge gives one session whichever factor completes it. A plugin's
// completion spends it for the code path, and of two plugin completions at
// once only one signs.
func TestCompleteMFAChallenge_OneChallengeGivesOneSession(t *testing.T) {
	t.Parallel()

	t.Run("a completed challenge is refused to the code path", func(t *testing.T) {
		t.Parallel()
		c := newChallengeCompletion(t)
		challenge := c.challenge(t, "acme", []string{"editor"})
		_, err := c.h.CompleteMFAChallenge(context.Background(), challenge)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(makeMFAVerifyBody(challenge, "123456")))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c.h.MFAVerify(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), string(i18n.CodeChallengeExpired))

		_, err = c.h.MFAChallengeAccount(context.Background(), challenge)
		assert.ErrorIs(t, err, core.ErrMFAChallengeInvalid)
	})

	t.Run("two completions at once sign once", func(t *testing.T) {
		t.Parallel()
		c := newChallengeCompletion(t)
		challenge := c.challenge(t, "acme", []string{"editor"})
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = c.h.CompleteMFAChallenge(context.Background(), challenge)
			}()
		}
		wg.Wait()
		signed := 0
		for _, err := range errs {
			if err == nil {
				signed++
				continue
			}
			assert.ErrorIs(t, err, core.ErrMFAChallengeInvalid)
		}
		assert.Equal(t, 1, signed)
	})
}

// A challenge completes only where its sign-in could have started, and only
// for an account that may still sign in there.
func TestCompleteMFAChallenge_Refusals(t *testing.T) {
	t.Parallel()

	t.Run("a challenge for another tenant on a mapped domain", func(t *testing.T) {
		t.Parallel()
		c := newChallengeCompletion(t)
		challenge := c.challenge(t, "acme", []string{"editor"})
		_, err := c.h.MFAChallengeAccount(onGlobexDomain(), challenge)
		assert.ErrorIs(t, err, core.ErrMFAChallengeInvalid)
		_, err = c.h.CompleteMFAChallenge(onGlobexDomain(), challenge)
		assert.ErrorIs(t, err, core.ErrMFAChallengeInvalid)
		_, err = c.h.CompleteMFAChallenge(context.Background(), challenge)
		assert.NoError(t, err, "the refusals spent nothing")
	})

	t.Run("a session token is no challenge", func(t *testing.T) {
		t.Parallel()
		c := newChallengeCompletion(t)
		session, err := auth.Sign(challengeCompletionSecret, 3600, c.user.ID, c.user.Email, []string{"editor"}, "acme", 3)
		require.NoError(t, err)
		_, err = c.h.CompleteMFAChallenge(context.Background(), session)
		assert.ErrorIs(t, err, core.ErrMFAChallengeInvalid)
	})

	t.Run("a membership revoked since the password step", func(t *testing.T) {
		t.Parallel()
		c := newChallengeCompletion(t)
		challenge := c.challenge(t, "globex", []string{"admin"})
		c.h.WithMemberships(newFakeMemberships())
		_, err := c.h.CompleteMFAChallenge(context.Background(), challenge)
		assert.ErrorIs(t, err, core.ErrMFAChallengeInvalid)
	})

	t.Run("a disabled account", func(t *testing.T) {
		t.Parallel()
		c := newChallengeCompletion(t)
		challenge := c.challenge(t, "acme", []string{"editor"})
		c.user.Disabled = true
		_, err := c.h.CompleteMFAChallenge(context.Background(), challenge)
		assert.ErrorIs(t, err, core.ErrAccountInactive)
	})
}

// completerHost records the challenge completer the admin router hands it.
type completerHost struct {
	core.Host
	mu        sync.Mutex
	completer core.MFAChallengeCompleter
}

func (h *completerHost) SetMFAChallengeCompleter(c core.MFAChallengeCompleter) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completer = c
}

// The admin router hands the engine host the completion its own code sign-in
// uses, so a challenge a plugin completes through the host is refused at
// mfa-verify.
func TestAdminRouter_HandsTheHostItsChallengeCompletion(t *testing.T) {
	host := &completerHost{}
	rg := newTokenRig(t, WithScalingHost(host), WithMFAStore(&fakeMFAStore{}))
	u, _ := rg.user("editor")
	challenge, err := auth.SignChallenge(rg.cfg.JWTSecret, u.ID, u.Email, u.Roles, "default")
	require.NoError(t, err)

	host.mu.Lock()
	completer := host.completer
	host.mu.Unlock()
	require.NotNil(t, completer, "the admin router wired no completion")
	_, err = completer.CompleteMFAChallenge(context.Background(), challenge)
	require.NoError(t, err)

	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/mfa-verify", body: map[string]string{"challenge_token": challenge, "code": "123456"}})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), string(i18n.CodeChallengeExpired))
}
