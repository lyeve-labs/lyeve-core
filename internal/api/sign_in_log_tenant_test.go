package api

import (
	"context"
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
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// On the install's shared host, mapped to the default tenant, an account of
// acme signs in while acme is missing from the roster. The warning is about
// acme, so it goes under acme, and nothing the default tenant's admins read
// names acme or the account's digest.
func TestWarnIfTenantMissing_FiledUnderTheAccountsTenant(t *testing.T) {
	const host, account = core.DefaultTenantSlug, "acme"
	ring, sink := engineLogs(t, host)
	digest := compliance.SubjectRef("person@example.com")

	h := &AuthHandler{multiTenant: true, tenantValidator: func(_ context.Context, tenant string) bool { return tenant != account }}
	signIn := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		h.warnIfTenantMissing(r.Context(), account, "person@example.com")
	})
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", nil)
	r = r.WithContext(core.WithTenantID(r.Context(), host))
	apimw.TenantHeader(true)(signIn).ServeHTTP(httptest.NewRecorder(), r)

	require.Equal(t, 1, countNaming(ring, digest), "the warning is still logged")
	for _, line := range visibleTo(t, host, ring, sink) {
		assert.NotContains(t, line, account)
		assert.NotContains(t, line, digest)
	}
	assert.Len(t, visibleTo(t, account, ring, sink), 2, "acme's ring and stored log hold it")
}

// failingWindows cannot read any tenant's provider window.
type failingWindows struct{}

func (failingWindows) ProviderWindowDays(context.Context, string) (int, error) {
	return 0, errors.New("provider window read failed")
}

func (failingWindows) ProviderWindowAdmitsAccount(context.Context, string, *time.Time) (bool, error) {
	return false, errors.New("provider window read failed")
}

// On the shared host, mapped to the default tenant, a provider account of acme
// signs in with a correct password and is refused by acme's provider window,
// or the window cannot be read. Each line names acme's account, so it goes
// under acme, and nothing the default tenant's admins read names it.
func TestPasswordSignInRefusal_FiledUnderTheAccountsTenant(t *testing.T) {
	const host, account = core.DefaultTenantSlug, "acme"
	ring, sink := engineLogs(t, host)

	longAgo := time.Now().AddDate(-2, 0, 0)
	cases := map[string]*AuthHandler{
		"outside the window": {},
		"window unreadable":  {providerWindows: failingWindows{}},
	}
	for name, h := range cases {
		user := &domain.User{ID: uuid.New(), TenantID: account, ProviderSeenAt: &longAgo}
		r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", nil)
		r = r.WithContext(core.WithTenantID(r.Context(), host))
		require.False(t, h.respondIfPasswordSignInRefused(httptest.NewRecorder(), r, user), name)
		require.Equal(t, 1, countNaming(ring, user.ID.String()), "%s: the line is still logged", name)
		for _, line := range visibleTo(t, host, ring, sink) {
			assert.NotContains(t, line, user.ID.String(), name)
		}
		mine := 0
		for _, line := range visibleTo(t, account, ring, sink) {
			if strings.Contains(line, user.ID.String()) {
				mine++
			}
		}
		assert.Equal(t, 2, mine, "%s: acme's ring and stored log hold it", name)
	}
}

// On the shared host, mapped to the default tenant, an account of acme
// presents its MFA challenge and the authenticator cannot be read. The line
// names acme's account, so it goes under acme, the tenant the challenge was
// issued for.
func TestMFAVerify_LinesFiledUnderTheChallengesTenant(t *testing.T) {
	const host, account = core.DefaultTenantSlug, "acme"
	const secret = "test-secret-at-least-32-bytes-long!!"
	ring, sink := engineLogs(t, host)

	h := NewAuthHandler(nil, &failingMFAStore{}, nil, secret, 3600, false)
	userID := uuid.New()
	challenge, err := auth.SignChallenge(secret, userID, "a@example.com", []string{"editor"}, account)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/mfa-verify", strings.NewReader(makeMFAVerifyBody(challenge, "123456")))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(core.WithTenantID(r.Context(), host))
	rec := httptest.NewRecorder()
	h.MFAVerify(rec, r)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

	require.Equal(t, 1, countNaming(ring, userID.String()), "the line is still logged")
	for _, line := range visibleTo(t, host, ring, sink) {
		assert.NotContains(t, line, userID.String())
	}
	assert.Len(t, visibleTo(t, account, ring, sink), 2, "acme's ring and stored log hold it")
}
