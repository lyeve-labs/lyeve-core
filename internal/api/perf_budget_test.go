//go:build !mutest

// Package api: endpoint performance budget tests.
//
// These tests measure p95 latency for critical API endpoints through the
// full middleware stack against a real PostgreSQL container. Budgets are
// set conservatively to catch regressions early.
//
// Skipped in short mode.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// Performance Budgets

type endpointBudget struct {
	name    string // human-readable name
	method  string
	path    string
	body    string
	p95ms   int // max allowed p95 latency in milliseconds
	needsDB bool
}

var perfBudgets = []endpointBudget{
	{name: "GET /api/admin/health", method: http.MethodGet, path: "/api/admin/health", p95ms: 50, needsDB: true},
	{name: "GET /api/admin/ready", method: http.MethodGet, path: "/api/admin/ready", p95ms: 50, needsDB: true},
	{name: "GET /api/admin/metrics", method: http.MethodGet, path: "/api/admin/metrics", p95ms: 100, needsDB: false},
	{name: "GET /api/admin/auth/me", method: http.MethodGet, path: "/api/admin/auth/me", p95ms: 200, needsDB: true},
	{name: "GET /api/admin/schemas", method: http.MethodGet, path: "/api/admin/schemas", p95ms: 200, needsDB: true},
	{name: "GET /api/admin/entitlements", method: http.MethodGet, path: "/api/admin/entitlements", p95ms: 150, needsDB: false},
	{name: "GET /api/admin/plugins/status", method: http.MethodGet, path: "/api/admin/plugins/status", p95ms: 150, needsDB: false},
	{name: "GET /api/admin/users", method: http.MethodGet, path: "/api/admin/users", p95ms: 200, needsDB: true},
}

const (
	perfWarmup = 20
	perfMeas   = 100
	perfEmail  = "perf-budget@example.com"
	perfPass   = "perf-budget-test-password-12"
)

func TestEndpointPerformanceBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping endpoint performance budget test in short mode (needs Docker)")
	}

	ctx := context.Background()
	pool := testdb.Postgres(t)
	defer pool.Close()

	cfg := &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecrets:       []string{"perf-budget-jwt-secret-32bytes!!"},
		JWTSecret:        "perf-budget-jwt-secret-32bytes!!",
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"http://localhost:5173"},
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
		RateLimitRPS:     0, // measure handler latency, not rate limiting
		SecureCookie:     false,
		PasswordHashAlgo: "bcrypt",
		SetupToken:       "perf-budget-setup-token-0123",
	}

	router, _ := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))

	jwt := perfSetupUser(t, router)
	require.NotEmpty(t, jwt, "failed to get JWT for perf budget tests")
	_ = ctx

	for _, b := range perfBudgets {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			samples := make([]time.Duration, 0, perfWarmup+perfMeas)

			for i := 0; i < perfWarmup+perfMeas; i++ {
				req := httptest.NewRequest(b.method, b.path, strings.NewReader(b.body))
				if b.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				req.Header.Set("Authorization", "Bearer "+jwt)
				req.RemoteAddr = "192.0.2.1:12345"

				rec := httptest.NewRecorder()
				start := time.Now()
				router.ServeHTTP(rec, req)
				elapsed := time.Since(start)
				samples = append(samples, elapsed)
			}

			meas := samples[perfWarmup:]
			p95 := durationP95(meas)

			t.Logf("p95=%s mean=%s budget=%dms (n=%d)",
				p95.Round(time.Millisecond),
				durationMean(meas).Round(time.Microsecond),
				b.p95ms, len(meas))

			assert.LessOrEqual(t, p95.Milliseconds(), int64(b.p95ms),
				"%s: p95 latency %s exceeds budget of %dms", b.name, p95.Round(time.Millisecond), b.p95ms)
		})
	}
}

// Helpers

func perfSetupUser(t *testing.T, router http.Handler) string {
	t.Helper()

	setupBody := fmt.Sprintf(`{"email":"%s","password":"%s","name":"Perf Tester"}`, perfEmail, perfPass)
	setupReq := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(setupBody))
	setupReq.Header.Set("Content-Type", "application/json")
	setupReq.Header.Set(SetupTokenHeader, "perf-budget-setup-token-0123")
	setupReq.RemoteAddr = "192.0.2.1:12345"
	setupRec := httptest.NewRecorder()
	router.ServeHTTP(setupRec, setupReq)
	t.Logf("setup response: %d - %s", setupRec.Code, setupRec.Body.String())

	loginBody := fmt.Sprintf(`{"email":"%s","password":"%s"}`, perfEmail, perfPass)
	loginReq := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.RemoteAddr = "192.0.2.1:12345"
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)

	var resp struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(loginRec.Body).Decode(&resp)

	// Fallback: token may be in Set-Cookie.
	if resp.Token == "" {
		for _, c := range loginRec.Result().Cookies() {
			if c.Name == "jwt" {
				resp.Token = c.Value
				break
			}
		}
	}

	if resp.Token == "" {
		t.Logf("login response: %d - body: %s", loginRec.Code, loginRec.Body.String())
	}

	return resp.Token
}

// durationP95 returns the p95 duration from a sorted slice.
func durationP95(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * 0.95)
	return sorted[idx]
}

func durationMean(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	var total time.Duration
	for _, s := range samples {
		total += s
	}
	return total / time.Duration(len(samples))
}
