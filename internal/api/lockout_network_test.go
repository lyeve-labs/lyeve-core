package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// passwordRoutes are the two routes that sign in with a password. They share
// one lockout, so a lock earned on one holds on the other.
var passwordRoutes = []struct {
	name string
	path string
	call func(*AuthHandler, http.ResponseWriter, *http.Request)
}{
	{"login", "/api/admin/auth/login", func(h *AuthHandler, w http.ResponseWriter, r *http.Request) { h.Login(w, r) }},
	{"token", "/api/v1/auth/token", func(h *AuthHandler, w http.ResponseWriter, r *http.Request) { h.Token(w, r) }},
}

// signInFrom sends one password sign-in from addr and returns the status.
func signInFrom(h *AuthHandler, path string, call func(*AuthHandler, http.ResponseWriter, *http.Request), addr, password string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(makeLoginBody("user@test.com", password)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = addr
	rr := httptest.NewRecorder()
	call(h, rr, req)
	return rr.Code
}

// Wrong passwords lock the network they came from out of the account, not
// the account itself. Anyone who knows an address can send five wrong
// passwords, and if that locked the account its owner could not sign in from
// anywhere. Addresses in one IPv6 /64 are one network, because a single host
// is routinely handed the whole prefix.
func TestLockout_WrongPasswordsLockOnlyTheirNetwork(t *testing.T) {
	t.Parallel()
	const correct = "correct-horse-battery-staple"
	for _, backend := range lockoutBackends {
		for _, route := range passwordRoutes {
			for _, tc := range []struct {
				name, attacker, sameNet, otherNet string
			}{
				{"ipv4", "203.0.113.7:4000", "203.0.113.7:4001", "198.51.100.9:4000"},
				{"ipv6", "[2001:db8:1:2::a]:4000", "[2001:db8:1:2::b]:4000", "[2001:db8:1:3::a]:4000"},
				{"ipv4-mapped ipv6", "[::ffff:203.0.113.7]:4000", "203.0.113.7:4001", "198.51.100.9:4000"},
			} {
				t.Run(backend+"/"+route.name+"/"+tc.name, func(t *testing.T) {
					t.Parallel()
					users, _ := oneUser(t)
					h := lockoutHandler(users, nil, backend)
					for i := 0; i < h.MaxFailedAttempts; i++ {
						require.Equal(t, http.StatusUnauthorized, signInFrom(h, route.path, route.call, tc.attacker, "wrong-password"), "attempt %d", i+1)
					}
					assert.Equal(t, http.StatusUnauthorized, signInFrom(h, route.path, route.call, tc.sameNet, correct),
						"the network that sent the wrong passwords stays locked out")
					assert.Equal(t, http.StatusOK, signInFrom(h, route.path, route.call, tc.otherNet, correct),
						"another network still signs in with the correct password")
				})
			}
		}
	}
}

// Wrong passwords spread over many networks still lock the account once they
// reach the account's ceiling, so a guesser cannot buy unlimited attempts with
// addresses.
func TestLockout_AccountCeilingLocksAcrossNetworks(t *testing.T) {
	t.Parallel()
	for _, backend := range lockoutBackends {
		for _, route := range passwordRoutes {
			t.Run(backend+"/"+route.name, func(t *testing.T) {
				t.Parallel()
				users, uid := oneUser(t)
				h := lockoutHandler(users, nil, backend)
				for n := 0; n < accountAttemptCeiling; n++ {
					addr := fmt.Sprintf("10.20.%d.%d:4000", n/250, n%250+1)
					require.Equal(t, http.StatusUnauthorized, signInFrom(h, route.path, route.call, addr, "wrong-password"), "attempt %d", n+1)
				}
				fresh := "198.51.100.200:4000"
				assert.Equal(t, http.StatusUnauthorized, signInFrom(h, route.path, route.call, fresh, "correct-horse-battery-staple"),
					"the account is locked once every network together reaches the ceiling")
				n, err := lockoutCount(h.lockoutChecker, networkAttemptKey(uid, requesterNet("198.51.100.200")))
				require.NoError(t, err)
				assert.Zero(t, n, "an attempt the ceiling refused was never checked, so its network keeps no count of it")
			})
		}
	}
}

// The password step-up confirms an admin token or a device sign-in approval,
// and only a caller already holding the account's session reaches it. Its
// wrong passwords are counted per account, whatever network they come from:
// counted per network, a stolen session could make five guesses from every
// network it switched to.
func TestStepUpPassword_WrongPasswordsLockAcrossNetworks(t *testing.T) {
	t.Parallel()
	for _, backend := range lockoutBackends {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			users, _ := oneUser(t)
			h := lockoutHandler(users, nil, backend)
			user := users.byEmail["user@test.com"]
			stepUp := func(n int, password string) int {
				req := httptest.NewRequest(http.MethodPost, "/api/admin/admin-tokens", nil)
				req.RemoteAddr = fmt.Sprintf("192.0.2.%d:4000", n+1)
				status, _ := h.stepUpPassword(req, user, password)
				return status
			}
			for n := 0; n < h.MaxFailedAttempts; n++ {
				require.Equal(t, http.StatusForbidden, stepUp(n, "wrong-password"), "attempt %d", n+1)
			}
			assert.Equal(t, http.StatusTooManyRequests, stepUp(h.MaxFailedAttempts, "wrong-password"),
				"a wrong password from yet another network is still refused once the account's step-up limit is spent")
			login := passwordRoutes[0]
			assert.Equal(t, http.StatusOK, signInFrom(h, login.path, login.call, "198.51.100.9:4000", "correct-horse-battery-staple"),
				"the step-up count does not lock password sign-in")
		})
	}
}
