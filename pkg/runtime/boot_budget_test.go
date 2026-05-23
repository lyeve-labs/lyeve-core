package runtime

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// bootBudget is how long the engine may take from RunWithOptions to serving
// a request, against a database already migrated. It boots in well under a
// second. The readiness gate's own timeout is five, so an engine that waits
// for something outside the process to open the gate fails here instead of in
// a benchmark.
const bootBudget = 3 * time.Second

func TestRun_ServesWithinBootBudget(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("postgres not selected")
	}
	_, dsn := testdb.PostgresWithDSN(t)

	admin, api := freeAddr(t), freeAddr(t)
	state := t.TempDir()
	for k, v := range map[string]string{
		"DATABASE_URL":            dsn,
		"DATABASE_REPLICA_URL":    "",
		"APP_ENV":                 "development",
		"ADMIN_LISTEN_ADDR":       admin,
		"API_LISTEN_ADDR":         api,
		"JWT_SECRET":              "boot-budget-jwt-secret-0123456789abcdef012345",
		"ENCRYPTION_KEY":          "boot-budget-encryption-key-distinct-0123456789",
		"JWT_KEY_PATH":            filepath.Join(state, "jwt_key.json"),
		"LYEVE_LICENSE_CACHE_DIR": state,
		"LYEVE_LICENSE_KEY":       "",
		"LYEVE_SETUP_TOKEN":       "boot-budget-setup-token-0001",
		"LYEVE_MODE":              "",
		"LYEVE_SETUP_MODE":        "",
		"LYEVE_PLUGINS":           "",
		"MIGRATIONS_PATH":         filepath.Join("..", "..", "migrations"),
		"STORAGE_LOCAL_PATH":      t.TempDir(),
	} {
		t.Setenv(k, v)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	began := time.Now()
	go func() { done <- RunWithOptions(ctx, Options{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Error("the engine did not stop")
		}
	})

	var served time.Duration
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.NoError(t, err, "the engine exited during boot")
		default:
		}
		resp, err := http.Get("http://" + admin + "/api/admin/setup")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		served = time.Since(began)
		return true
	}, 30*time.Second, 10*time.Millisecond, "the engine never served /api/admin/setup")

	t.Logf("served after %v", served.Round(time.Millisecond))
	assert.Less(t, served, bootBudget,
		"the engine took %v to serve its first request, over the %v budget: look for a gate waiting on a timeout, a network call to a default address with retries, or work repeated per caller",
		served.Round(time.Millisecond), bootBudget)
}
