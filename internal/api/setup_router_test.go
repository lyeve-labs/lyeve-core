//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

const routerSetupToken = "router-setup-token-0123456789"

func setupRouterConfig() *config.Config {
	const secret = "setup-router-test-secret-32-bytes!"
	return &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecret:        secret,
		JWTSecrets:       []string{secret},
		JWTExpirySecs:    3600,
		MaxBodyBytes:     1 << 20,
		MaxJSONBodyBytes: 1 << 20,
		PasswordHashAlgo: "bcrypt",
		SetupToken:       routerSetupToken,
	}
}

func postSetup(t *testing.T, router http.Handler, email, token string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":"CorrectHorseBatteryStaple1!","setup_token":%q}`, email, token)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// The router reads LYEVE_SETUP_TOKEN from the config, so this proves the
// control is wired, not only present on the handler.
func TestAdminRouterSetup_RequiresConfiguredToken(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database container")
	}
	pool := testdb.Postgres(t)
	router, err := NewAdminRouter(pool, setupRouterConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)

	statusReq := httptest.NewRequest(http.MethodGet, "/api/admin/setup", nil)
	statusRec := httptest.NewRecorder()
	router.ServeHTTP(statusRec, statusReq)
	require.Equal(t, http.StatusOK, statusRec.Code)
	var status map[string]any
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &status))
	assert.Equal(t, true, status["setup_required"])
	assert.Equal(t, SetupTokenFromEnv, status["token_source"])

	assert.Equal(t, http.StatusUnauthorized, postSetup(t, router, "intruder@example.test", "").Code)
	assert.Equal(t, http.StatusUnauthorized, postSetup(t, router, "intruder@example.test", "router-setup-token-guess").Code)
	n, err := db.NewUserStore(pool).Count(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "a refused claim created an account")

	require.Equal(t, http.StatusCreated, postSetup(t, router, "owner@example.test", routerSetupToken).Code)
	assert.Equal(t, http.StatusUnauthorized, postSetup(t, router, "late@example.test", routerSetupToken).Code,
		"the token must stop working once the first super admin exists")
}

// Setup nobody wired a token for stays closed: an empty LYEVE_SETUP_TOKEN and
// no runtime-generated token refuses every claim.
func TestAdminRouterSetup_ClosedWithoutAnyToken(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database container")
	}
	pool := testdb.Postgres(t)
	cfg := setupRouterConfig()
	cfg.SetupToken = ""
	router, err := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
	require.NoError(t, err)

	assert.Equal(t, http.StatusUnauthorized, postSetup(t, router, "owner@example.test", "").Code)
	assert.Equal(t, http.StatusUnauthorized, postSetup(t, router, "owner@example.test", routerSetupToken).Code)
}

func TestAdminRouterSetup_ConcurrentClaimsYieldOneSuperAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a database container")
	}
	const callers = 8
	pool := testdb.PostgresWithMaxConns(t, callers+4)
	router, err := NewAdminRouter(pool, setupRouterConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)

	start := make(chan struct{})
	codes := make([]int, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = postSetup(t, router, fmt.Sprintf("owner%d@example.test", i), routerSetupToken).Code
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		}
	}
	var admins int
	row, err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sys_users WHERE 'super_admin' = ANY(roles)`)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&admins))
	assert.Equal(t, 1, created, "status codes: %v", codes)
	assert.Equal(t, 1, admins)
}
