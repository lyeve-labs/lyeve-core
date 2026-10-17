//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// hostSignIn holds one dialect's accounts for the sign-in tests below. Every
// account's home is acme. member also belongs to globex, as an admin there.
type hostSignIn struct {
	users   *db.UserStore
	handler *AuthHandler
	secret  string
	hash    string
	acme    string
	member  string
	super   string
	globex  string
}

func newHostSignIn(t *testing.T, pool db.DB) *hostSignIn {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword("bcrypt", providerTestPassword)
	require.NoError(t, err)
	users := db.NewUserStore(pool)
	members := newFakeMemberships()
	s := &hostSignIn{users: users, secret: "test-secret-at-least-32-bytes-long!!", hash: hash}
	s.handler = NewAuthHandler(users, nil, pool, s.secret, 3600, false)
	s.handler.WithMemberships(members)
	s.handler.WithRiskBasedMFADisabled(true)

	s.acme = s.account(t, "acme", []string{"editor"})
	s.member = s.account(t, "acme", []string{"editor"})
	s.super = s.account(t, "acme", []string{"super_admin"})
	s.globex = s.account(t, "globex", []string{"editor"})
	m, err := users.GetByEmail(ctx, s.member)
	require.NoError(t, err)
	_, err = members.Grant(ctx, m.ID, "globex", []string{"admin"})
	require.NoError(t, err)
	return s
}

// account creates a password account and returns its address.
func (s *hostSignIn) account(t *testing.T, home string, roles []string) string {
	t.Helper()
	address := "host-" + uuid.NewString()[:8] + "@example.com"
	_, err := s.users.Create(context.Background(), address, s.hash, roles, home)
	require.NoError(t, err)
	return address
}

// signIn is one password sign-in through the tenant middleware.
type signIn struct {
	code    int
	body    string
	header  http.Header
	cookie  bool
	tenant  string
	roles   []string
	elapsed time.Duration
}

// host names where a request arrives: a mapped domain, the instance host of
// an install with one tenant, or the instance host of one with several.
type host func(context.Context) context.Context

var (
	globexDomain   host = func(ctx context.Context) context.Context { return core.WithTenantID(ctx, "globex") }
	soleTenantHost host = func(ctx context.Context) context.Context { return core.WithFallbackTenantID(ctx, "acme") }
	instanceHost   host = func(ctx context.Context) context.Context { return ctx }
)

// signInDoorsNames are the engine's two password sign-ins.
var signInDoorsNames = []string{"login", "token"}

func (s *hostSignIn) door(name string) http.HandlerFunc {
	if name == "token" {
		return s.handler.Token
	}
	return s.handler.Login
}

// send posts a password sign-in on h, the way the router delivers it after
// the host resolver and the tenant middleware ran.
func (s *hostSignIn) send(t *testing.T, door string, h host, email, password, bodyTenant string) signIn {
	t.Helper()
	payload := map[string]string{"email": email, "password": password}
	if bodyTenant != "" {
		payload["tenant"] = bodyTenant
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(h(req.Context()))
	rec := httptest.NewRecorder()
	start := time.Now()
	apimw.TenantHeader(true)(s.door(door)).ServeHTTP(rec, req)
	out := signIn{code: rec.Code, body: rec.Body.String(), header: rec.Header(), cookie: len(rec.Result().Cookies()) > 0, elapsed: time.Since(start)}
	if rec.Code == http.StatusOK {
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		tok, _ := resp["token"].(string)
		claims, err := auth.Parse(s.secret, tok)
		require.NoError(t, err, "body=%q", rec.Body.String())
		out.tenant, out.roles = claims.TenantID, claims.Roles
	}
	return out
}

// On a tenant's mapped domain an account that is neither at home there, nor a
// member, nor a super admin is an unknown address. With the right password or
// a wrong one it gets the answer an unknown address gets, in status, body,
// headers, cookies and timing, and no session.
func TestPasswordSignIn_AnotherTenantsAccountOnADomainAnswersAsAnUnknownAddress(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		s := newHostSignIn(t, pool)
		for _, door := range signInDoorsNames {
			foreign := s.account(t, "acme", []string{"editor"})
			unknown := "host-" + uuid.NewString()[:8] + "@example.com"
			var foreignTimes, unknownTimes []time.Duration
			for i := 0; i <= s.handler.MaxFailedAttempts; i++ {
				for _, password := range []string{providerTestPassword, "not-the-password"} {
					want := s.send(t, door, globexDomain, unknown, password, "")
					require.Equal(t, http.StatusUnauthorized, want.code, door)
					got := s.send(t, door, globexDomain, foreign, password, "")
					assert.Equal(t, want.code, got.code, "%s: status", door)
					assert.Equal(t, want.body, got.body, "%s: body", door)
					assert.Equal(t, want.header, got.header, "%s: headers", door)
					assert.False(t, got.cookie, "%s: a session cookie was set", door)
					foreignTimes = append(foreignTimes, got.elapsed)
					unknownTimes = append(unknownTimes, want.elapsed)
				}
			}
			ratio := float64(median(foreignTimes)) / float64(median(unknownTimes))
			assert.True(t, ratio >= 0.5 && ratio <= 2.0, "%s: timing ratio %.2f, unknown address %v", door, ratio, median(unknownTimes))
		}
	})
}

// tracingRiskAssessor records the device history attempts and the risk
// assessments a sign-in asks for.
type tracingRiskAssessor struct {
	*recordingRiskAssessor
	assessed []uuid.UUID
}

func (a *tracingRiskAssessor) Assess(_ context.Context, userID uuid.UUID, _ security.DeviceFingerprint) (*security.RiskAssessment, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.assessed = append(a.assessed, userID)
	return &security.RiskAssessment{}, nil
}

func (a *tracingRiskAssessor) assessedFor(userID uuid.UUID) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, id := range a.assessed {
		if id == userID {
			n++
		}
	}
	return n
}

// An unknown address leaves nothing behind, so neither may another tenant's
// account on a tenant's mapped domain: no failed attempt counted, no device
// history row, no risk assessment and no log record, under either tenant.
func TestPasswordSignIn_AnotherTenantsAccountOnADomainLeavesNoTrace(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		ctx := context.Background()
		s := newHostSignIn(t, pool)
		devices := &tracingRiskAssessor{recordingRiskAssessor: &recordingRiskAssessor{}}
		s.handler.WithDeviceRiskAssessor(devices)
		logsFor := captureStampedLogs(t)
		for _, door := range signInDoorsNames {
			address := s.account(t, "acme", []string{"editor"})
			u, err := s.users.GetByEmail(ctx, address)
			require.NoError(t, err)
			for i := 0; i <= s.handler.MaxFailedAttempts; i++ {
				s.send(t, door, globexDomain, address, "not-the-password", "")
				s.send(t, door, globexDomain, address, providerTestPassword, "")
			}
			count, err := s.handler.lockoutChecker.CountRecentFailures(ctx, u.ID, time.Now().Add(-s.handler.LockoutWindow))
			require.NoError(t, err)
			assert.Zero(t, count, "%s: failed attempts counted", door)
			assert.Empty(t, devices.tenantsFor(u.ID), "%s: device history rows", door)
			assert.Zero(t, devices.assessedFor(u.ID), "%s: risk assessments", door)
			assert.Empty(t, logsFor(u.ID), "%s: log records naming the account", door)
		}
	})
}

// Wrong passwords on a tenant's mapped domain never lock another tenant's
// account: it still signs in with its password on the instance host.
func TestPasswordSignIn_AnotherTenantsAccountCannotBeLockedFromADomain(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		s := newHostSignIn(t, pool)
		for _, door := range signInDoorsNames {
			address := s.account(t, "acme", []string{"editor"})
			for i := 0; i < 2*s.handler.MaxFailedAttempts; i++ {
				s.send(t, door, globexDomain, address, "not-the-password", "")
			}
			got := s.send(t, door, instanceHost, address, providerTestPassword, "")
			assert.Equal(t, http.StatusOK, got.code, "%s: refused after wrong passwords on globex's domain", door)
			assert.Equal(t, "acme", got.tenant, door)
		}
	})
}

// On a tenant's mapped domain the sign-in acts in that tenant: its own
// accounts, a member with the membership's roles, and a super admin. A body
// tenant naming another tenant is refused as a wrong password is.
func TestPasswordSignIn_OnADomainActsInTheDomainsTenant(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		s := newHostSignIn(t, pool)
		for _, door := range signInDoorsNames {
			home := s.send(t, door, globexDomain, s.globex, providerTestPassword, "")
			assert.Equal(t, http.StatusOK, home.code, "%s: globex's own account", door)
			assert.Equal(t, "globex", home.tenant, door)

			member := s.send(t, door, globexDomain, s.member, providerTestPassword, "")
			assert.Equal(t, http.StatusOK, member.code, "%s: a member of globex", door)
			assert.Equal(t, "globex", member.tenant, door)
			assert.Equal(t, []string{"admin"}, member.roles, "%s: the membership's roles", door)

			named := s.send(t, door, globexDomain, s.member, providerTestPassword, "globex")
			assert.Equal(t, http.StatusOK, named.code, "%s: a body tenant naming the domain's own", door)
			assert.Equal(t, "globex", named.tenant, door)

			super := s.send(t, door, globexDomain, s.super, providerTestPassword, "")
			assert.Equal(t, http.StatusOK, super.code, "%s: a super admin", door)
			assert.Equal(t, "globex", super.tenant, door)

			wrong := s.send(t, door, globexDomain, s.member, "not-the-password", "")
			other := s.send(t, door, globexDomain, s.member, providerTestPassword, "acme")
			assert.Equal(t, wrong.code, other.code, "%s: a body tenant naming acme", door)
			assert.Equal(t, wrong.body, other.body, "%s: a body tenant naming acme", door)
			assert.False(t, other.cookie, "%s: a session cookie was set", door)
		}
	})
}

// A host that names no tenant (the instance's own host, with one tenant or
// several) signs in as before: the home tenant, or the body's tenant for a
// member or a super admin.
func TestPasswordSignIn_OnTheInstanceHostIsUnchanged(t *testing.T) {
	eachProviderDialect(t, func(t *testing.T, pool db.DB) {
		s := newHostSignIn(t, pool)
		hosts := map[string]host{"instance host": instanceHost, "sole tenant fallback": soleTenantHost}
		for hostName, h := range hosts {
			for _, door := range signInDoorsNames {
				at := fmt.Sprintf("%s on the %s", door, hostName)
				for _, address := range []string{s.acme, s.member, s.super} {
					got := s.send(t, door, h, address, providerTestPassword, "")
					assert.Equal(t, http.StatusOK, got.code, at)
					assert.Equal(t, "acme", got.tenant, at)
				}
				globex := s.send(t, door, h, s.globex, providerTestPassword, "")
				assert.Equal(t, http.StatusOK, globex.code, at)
				assert.Equal(t, "globex", globex.tenant, at)

				member := s.send(t, door, h, s.member, providerTestPassword, "globex")
				assert.Equal(t, http.StatusOK, member.code, at)
				assert.Equal(t, "globex", member.tenant, at)
				assert.Equal(t, []string{"admin"}, member.roles, at)

				super := s.send(t, door, h, s.super, providerTestPassword, "globex")
				assert.Equal(t, http.StatusOK, super.code, at)
				assert.Equal(t, "globex", super.tenant, at)

				wrong := s.send(t, door, h, s.acme, "not-the-password", "")
				outsider := s.send(t, door, h, s.acme, providerTestPassword, "globex")
				assert.Equal(t, wrong.code, outsider.code, "%s: a non-member naming globex", at)
				assert.Equal(t, wrong.body, outsider.body, "%s: a non-member naming globex", at)
			}
		}
	})
}

// An MFA challenge completes only where its sign-in could have started. On a
// tenant's mapped domain a challenge for another tenant is refused before the
// second factor is read, and elsewhere it goes on to the factor check.
func TestMFAVerify_ChallengeForAnotherTenantOnADomainIsRefused(t *testing.T) {
	t.Parallel()
	h := NewAuthHandler(&fakeUserStore{}, &failingMFAStore{}, nil, "secret", 3600, false)
	verify := func(at host, tenant string) (int, string) {
		challenge, err := auth.SignChallenge("secret", uuid.New(), "a@example.com", []string{"editor"}, tenant)
		require.NoError(t, err)
		body := fmt.Sprintf(`{"challenge_token":%q,"code":"123456"}`, challenge)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(at(req.Context()))
		rec := httptest.NewRecorder()
		apimw.TenantHeader(true)(http.HandlerFunc(h.MFAVerify)).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := verify(globexDomain, "acme")
	assert.Equal(t, http.StatusUnauthorized, code)
	assert.Contains(t, body, string(i18n.CodeChallengeExpired))

	for name, at := range map[string]host{"the domain's own tenant": globexDomain, "the instance host": instanceHost} {
		tenant := "acme"
		if name == "the domain's own tenant" {
			tenant = "globex"
		}
		code, body := verify(at, tenant)
		assert.Equal(t, http.StatusUnauthorized, code, name)
		assert.Contains(t, body, string(i18n.CodeMFANotConfigured), "%s: the factor check never ran", name)
	}
}
