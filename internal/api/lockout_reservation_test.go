package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// burstSize is how many wrong answers for one account arrive at once. Each
// comes from its own address, so the per-address rate limit never decides
// the outcome and only the account lockout can.
const burstSize = 50

// lockoutBackends are the two places the engine keeps its lockouts: the
// process, and the cache backend every replica shares.
var lockoutBackends = []string{"in-process", "shared"}

// roundTripBackend is the in-memory cache backend with the delay of a network
// round trip on every read, which a shared backend has in production. Without
// it a read and the write after it run back to back, and a burst reproduces
// the gap between them only now and then.
type roundTripBackend struct {
	*auth.MemoryBackend
}

func (b roundTripBackend) Get(ctx context.Context, key string) ([]byte, error) {
	time.Sleep(2 * time.Millisecond)
	return b.MemoryBackend.Get(ctx, key)
}

// newLockoutRig builds the admin and content API routers over one database,
// with a pool wide enough for the whole burst to reach the handlers at once.
// For the shared backend both routers get the same one, as the runtime
// hands them.
func newLockoutRig(t *testing.T, backend string, adminOpts ...RouterOption) *tokenRig {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	pool := testdb.PostgresWithMaxConns(t, burstSize)
	cfg := testConfig()
	common := []RouterOption{WithLifetime(testLifetime(t))}
	if backend == "shared" {
		common = append(common, WithLockoutBackend(auth.NewFlushProtectedBackend(roundTripBackend{auth.NewMemoryBackend()})))
	}
	tokens := newFakeAdminTokens(pool)
	all := append([]RouterOption{WithAdminTokenStore(tokens)}, common...)
	admin, err := NewAdminRouter(pool, cfg, append(all, adminOpts...)...)
	require.NoError(t, err)
	apiRouter, err := NewAPIRouter(pool, cfg, nil, common...)
	require.NoError(t, err)
	return &tokenRig{t: t, pool: pool, cfg: cfg, admin: admin, api: apiRouter, users: db.NewUserStore(pool), tokens: tokens}
}

// burst sends burstSize requests at the same instant and counts the status
// codes they get.
func burst(rg *tokenRig, call func(i int) tokenCall) map[int]int {
	calls := make([]tokenCall, burstSize)
	for i := range calls {
		calls[i] = call(i)
		calls[i].remote = fmt.Sprintf("10.9.%d.%d:4000", i/250, i%250+1)
	}
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
	)
	codes := map[int]int{}
	start.Add(1)
	for _, c := range calls {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			code := rg.do(c).Code
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()
	return codes
}

func adminTokenBody(extra map[string]any) map[string]any {
	b := map[string]any{
		"name":       "ci",
		"grants":     []string{core.AdminGrantSchemasRead},
		"expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// Concurrent wrong passwords for one account reach the password check no
// more often than the lockout allows, on each route that checks a password,
// whether the count is kept in the process or in the shared backend. Every
// request still gets the answer that route gives a wrong password or a
// locked account.
func TestLockout_ConcurrentWrongPasswordsReachTheCheckAtMostTheLimit(t *testing.T) {
	routes := []string{"login", "token", "admin token step-up"}
	for _, backend := range lockoutBackends {
		for _, route := range routes {
			t.Run(backend+"/"+route, func(t *testing.T) {
				rg := newLockoutRig(t, backend)
				u, session := rg.user("admin")
				logs := captureStampedLogs(t)

				codes := burst(rg, func(int) tokenCall {
					creds := map[string]any{"email": u.Email, "password": "wrong-password"}
					switch route {
					case "login":
						return tokenCall{method: http.MethodPost, path: "/api/admin/auth/login", body: creds}
					case "token":
						return tokenCall{method: http.MethodPost, path: "/api/v1/auth/token", body: creds, router: rg.api}
					default:
						return tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session,
							body: adminTokenBody(map[string]any{"password": "wrong-password"})}
					}
				})

				var reached int
				switch route {
				case "login", "token":
					// Both answers are the same 401 by design, so the line the
					// handler writes after a password fails is what tells a
					// checked password from a refused one.
					msg := "login failed: password did not verify"
					if route == "token" {
						msg = "token login failed: password did not verify"
					}
					for _, r := range logs(u.ID) {
						if r.message == msg {
							reached++
						}
					}
					assert.Equal(t, map[int]int{http.StatusUnauthorized: burstSize}, codes)
				default:
					reached = codes[http.StatusForbidden]
					assert.Equal(t, burstSize, codes[http.StatusForbidden]+codes[http.StatusTooManyRequests], "codes: %v", codes)
				}
				assert.Positive(t, reached, "codes: %v", codes)
				assert.LessOrEqual(t, reached, DefaultMaxFailedAttempts,
					"%d of %d concurrent wrong passwords were checked against a limit of %d", reached, burstSize, DefaultMaxFailedAttempts)
			})
		}
	}
}

// wrongTOTPCode returns a six digit code that no step near now accepts for
// secret, so a burst spread over a step boundary still sends only wrong codes.
func wrongTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	valid := map[string]bool{}
	for _, d := range []time.Duration{-time.Minute, -30 * time.Second, 0, 30 * time.Second, time.Minute} {
		c, err := totp.GenerateCode(secret, time.Now().Add(d))
		require.NoError(t, err)
		valid[c] = true
	}
	for n := 0; ; n++ {
		c := fmt.Sprintf("%06d", n)
		if !valid[c] {
			return c
		}
	}
}

// Concurrent wrong authenticator codes reach the code check no more often
// than the MFA lockout allows: one challenge spent from many requests stops
// at the challenge's attempts, fresh challenges stop at the account's
// failures, and the admin token step-up stops at the account's failures.
func TestMFALockout_ConcurrentWrongCodesReachTheCheckAtMostTheLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit int
	}{
		{"sign-in, one challenge", DefaultMFAChallengeMaxAttempts},
		{"sign-in, a challenge each", DefaultMFAVerifyMaxAttempts},
		{"admin token step-up", DefaultMFAVerifyMaxAttempts},
	}
	for _, backend := range lockoutBackends {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				key, err := totp.Generate(totp.GenerateOpts{Issuer: "LyEve", AccountName: "owner@example.com"})
				require.NoError(t, err)
				enc, err := security.EncryptSecret(key.Secret(), testConfig().JWTSecret)
				require.NoError(t, err)
				rg := newLockoutRig(t, backend, WithMFAStore(&mfaEnrolledStore{enc: enc}))
				u, session := rg.user("admin")
				code := wrongTOTPCode(t, key.Secret())
				challenge := func() string {
					c, err := auth.SignChallenge(rg.cfg.JWTSecret, u.ID, u.Email, u.Roles, "default")
					require.NoError(t, err)
					return c
				}
				shared := challenge()
				fresh := make([]string, burstSize)
				for i := range fresh {
					fresh[i] = challenge()
				}

				codes := burst(rg, func(i int) tokenCall {
					switch tc.name {
					case "sign-in, one challenge":
						return tokenCall{method: http.MethodPost, path: "/api/admin/auth/mfa-verify",
							body: map[string]any{"challenge_token": shared, "code": code}}
					case "sign-in, a challenge each":
						return tokenCall{method: http.MethodPost, path: "/api/admin/auth/mfa-verify",
							body: map[string]any{"challenge_token": fresh[i], "code": code}}
					default:
						return tokenCall{method: http.MethodPost, path: "/api/admin/admin-tokens", bearer: session,
							body: adminTokenBody(map[string]any{"mfa_code": code})}
					}
				})

				wrong := http.StatusUnprocessableEntity
				if tc.name == "admin token step-up" {
					wrong = http.StatusForbidden
				}
				reached := codes[wrong]
				assert.Equal(t, burstSize, reached+codes[http.StatusTooManyRequests], "codes: %v", codes)
				assert.Positive(t, reached, "codes: %v", codes)
				assert.LessOrEqual(t, reached, tc.limit,
					"%d of %d concurrent wrong codes were checked against a limit of %d", reached, burstSize, tc.limit)
			})
		}
	}
}

// lockoutHandler is an auth handler with its lockouts in the process or in a
// shared backend.
func lockoutHandler(users userStore, mfa security.MFAStore, backend string) *AuthHandler {
	h := NewAuthHandler(users, mfa, nil, lockoutTestSecret, 3600, false)
	if backend == "shared" {
		h.WithSharedLockoutBackend(auth.NewFlushProtectedBackend(auth.NewMemoryBackend()))
	}
	return h
}
