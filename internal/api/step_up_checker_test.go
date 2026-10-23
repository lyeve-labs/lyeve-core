package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// stepUpHost records the step-up checks the admin router hands it.
type stepUpHost struct {
	core.Host
	mu      sync.Mutex
	checker core.StepUpChecker
}

func (h *stepUpHost) SetStepUpChecker(c core.StepUpChecker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checker = c
}

func (h *stepUpHost) wired(t *testing.T) core.StepUpChecker {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotNil(t, h.checker, "the admin router wired no step-up checks")
	return h.checker
}

// stepUpRequest is a plugin route's request from u's signed-in session.
func stepUpRequest(u *domain.User, claims ...func(*core.AuthClaims)) *http.Request {
	return stepUpRequestIn("default", u, claims...)
}

// stepUpRequestIn is stepUpRequest on a request that resolved to tenant.
func stepUpRequestIn(tenant string, u *domain.User, claims ...func(*core.AuthClaims)) *http.Request {
	c := &core.AuthClaims{UserID: u.ID.String(), TenantID: tenant, Roles: u.Roles}
	for _, f := range claims {
		f(c)
	}
	ctx := context.WithValue(core.WithTenantID(context.Background(), tenant), core.ClaimsKey, c)
	return httptest.NewRequest(http.MethodPost, "/api/v1/plugin-route", nil).WithContext(ctx)
}

// A plugin checks the account's password through the handler the token
// routes use: an empty answer is never counted, a wrong one is, and the login
// lockout stops the check once it is reached. An account with no password
// cannot give one.
func TestCheckStepUp_Password(t *testing.T) {
	host := &stepUpHost{}
	rg := newTokenRig(t, WithScalingHost(host))
	checker := host.wired(t)
	u, _ := rg.user("user")

	assert.NoError(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpPassword, tokenTestPassword))
	for range 10 {
		assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpPassword, ""), core.ErrStepUpRequired)
	}
	for i := range 5 {
		assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpPassword, "wrong"), core.ErrStepUpRefused, "attempt %d", i)
	}
	assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpPassword, tokenTestPassword), core.ErrStepUpLocked,
		"the right password once the login lockout is reached")

	for _, hash := range []string{"", auth.OAuthOnlyMarker, auth.ExternalIssuerMarker} {
		nopw, err := rg.users.Create(context.Background(), "nopw-"+time.Now().Format("150405.000000")+"@example.com", hash, []string{"user"}, "default")
		require.NoError(t, err)
		assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(nopw), core.StepUpPassword, "anything"), core.ErrStepUpUnavailable, "hash %q", hash)
		assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(nopw), core.StepUpPassword, ""), core.ErrStepUpUnavailable, "hash %q", hash)
	}
}

// A plugin checks a current authenticator code the way the token routes do:
// a wrong code is counted, an accepted code is not accepted again in its
// window, and an account with no authenticator cannot give one.
func TestCheckStepUp_Code(t *testing.T) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "LyEve", AccountName: "owner@example.com"})
	require.NoError(t, err)
	enc, err := security.EncryptSecret(key.Secret(), testConfig().JWTSecret)
	require.NoError(t, err)

	host := &stepUpHost{}
	rg := newTokenRig(t, WithScalingHost(host), WithMFAStore(&mfaEnrolledStore{enc: enc}))
	checker := host.wired(t)
	u, _ := rg.user("user")

	assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpCode, " "), core.ErrStepUpRequired)
	assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpCode, "000000"), core.ErrStepUpRefused)
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	require.NoError(t, err)
	assert.NoError(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpCode, code))
	assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u), core.StepUpCode, code), core.ErrStepUpRefused, "a replayed code")

	for name, store := range map[string]security.MFAStore{"none enrolled": &fakeMFAStore{}, "passkey only": &passkeyOnlyStore{}} {
		host := &stepUpHost{}
		rg := newTokenRig(t, WithScalingHost(host), WithMFAStore(store))
		u, _ := rg.user("user")
		assert.ErrorIs(t, host.wired(t).CheckStepUp(stepUpRequest(u), core.StepUpCode, "123456"), core.ErrStepUpUnavailable, name)
	}
}

// Only a signed-in session has a person at it to ask. An API key or an admin
// token is refused before any answer is read, and a factor the engine does
// not check is an error, never a pass.
func TestCheckStepUp_OnlyASessionAndAKnownFactor(t *testing.T) {
	host := &stepUpHost{}
	rg := newTokenRig(t, WithScalingHost(host))
	checker := host.wired(t)
	u, _ := rg.user("user")

	for name, f := range map[string]func(*core.AuthClaims){
		"api key":     func(c *core.AuthClaims) { c.IsAPIKey = true },
		"admin token": func(c *core.AuthClaims) { c.AdminTokenID = "tok" },
		"no account":  func(c *core.AuthClaims) { c.UserID = "not-a-uuid" },
	} {
		assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(u, f), core.StepUpPassword, tokenTestPassword), core.ErrStepUpUnavailable, name)
	}
	err := checker.CheckStepUp(stepUpRequest(u), "email", "123456")
	require.ErrorIs(t, err, core.ErrServiceUnavailable)
	for _, known := range []error{core.ErrStepUpUnavailable, core.ErrStepUpRequired, core.ErrStepUpRefused, core.ErrStepUpLocked} {
		assert.NotErrorIs(t, err, known)
	}
}

// A plugin can set the session claims on its own request, so claims naming an
// account of another tenant reach no check and spend none of that account's
// lockouts. A super admin with no home tenant acts in every tenant.
func TestCheckStepUp_OnlyTheRequestTenantsAccount(t *testing.T) {
	host := &stepUpHost{}
	rg := newTokenRig(t, WithScalingHost(host))
	checker := host.wired(t)
	other, _ := rg.userIn("other-tenant", "user")

	for i := range 10 {
		assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(other), core.StepUpPassword, "wrong"), core.ErrStepUpUnavailable, "attempt %d", i)
	}
	assert.ErrorIs(t, checker.CheckStepUp(stepUpRequest(other), core.StepUpPassword, tokenTestPassword), core.ErrStepUpUnavailable)
	assert.NoError(t, checker.CheckStepUp(stepUpRequestIn("other-tenant", other), core.StepUpPassword, tokenTestPassword),
		"the account's own tenant, with no lockout spent by the refused checks")

	admin, _ := rg.userIn("", "super_admin")
	assert.NoError(t, checker.CheckStepUp(stepUpRequestIn("other-tenant", admin), core.StepUpPassword, tokenTestPassword))
	homeless, _ := rg.userIn("", "user")
	assert.NoError(t, checker.CheckStepUp(stepUpRequest(homeless), core.StepUpPassword, tokenTestPassword))
	assert.ErrorIs(t, checker.CheckStepUp(stepUpRequestIn("other-tenant", homeless), core.StepUpPassword, tokenTestPassword), core.ErrStepUpUnavailable)
}
