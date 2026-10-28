package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

var storedSettingsChild = flag.Bool("stored-settings-child", false, "boot the engine for TestRun_StoredTokenLifetimeAppliesAtTheNextStart, set only on its child process")

const storedSettingsEncryptionKey = "stored-settings-encryption-key-distinct-0123"

// A token lifetime saved in the console is read by Load before the database is
// open. The next start has to pick it up, or the save never applies at all.
// The engine boots in a child process, because a process holds one engine's
// registries and a second boot in this test binary would collide with them.
func TestRun_StoredTokenLifetimeAppliesAtTheNextStart(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the engine")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("postgres not selected")
	}
	pool, dsn := testdb.PostgresWithDSN(t)
	ctx := core.WithTenantID(context.Background(), "default")
	store := db.NewPluginConfigStore(pool).WithSealer(db.NewConfigSealer(storedSettingsEncryptionKey))
	_, err := store.Merge(ctx, config.CoreSettingsOwner, map[string]any{"JWT_EXPIRY_SECS": "1800"}, nil)
	require.NoError(t, err)

	cmd := exec.Command(os.Args[0],
		"-test.run=^TestRun_StoredTokenLifetimeChild$", "-test.count=1", "-test.timeout=5m", "-stored-settings-child")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "DATABASE_URL=" + dsn}
	for _, k := range []string{"TMPDIR", "GOCOVERDIR", "GORACE", "GOTRACEBACK"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Run(); err != nil {
		t.Fatalf("the child failed: %v\n%s", err, lastLines(logs.String(), 80))
	}
}

// TestRun_StoredTokenLifetimeChild is the engine the test above boots. It
// runs only as that test's child process.
func TestRun_StoredTokenLifetimeChild(t *testing.T) {
	if !*storedSettingsChild {
		t.Skip("runs only as the child process of TestRun_StoredTokenLifetimeAppliesAtTheNextStart")
	}
	dsn := os.Getenv("DATABASE_URL")
	admin := freeAddr(t)
	state := t.TempDir()
	for k, v := range map[string]string{
		"DATABASE_URL":            dsn,
		"DATABASE_REPLICA_URL":    "",
		"APP_ENV":                 "development",
		"ADMIN_LISTEN_ADDR":       admin,
		"API_LISTEN_ADDR":         freeAddr(t),
		"JWT_SECRET":              "stored-settings-jwt-secret-0123456789abcdef01",
		"ENCRYPTION_KEY":          storedSettingsEncryptionKey,
		"JWT_KEY_PATH":            filepath.Join(state, "jwt_key.json"),
		"LYEVE_LICENSE_CACHE_DIR": state,
		"LYEVE_LICENSE_KEY":       "",
		"LYEVE_SETUP_TOKEN":       "stored-settings-setup-token-01",
		"LYEVE_MODE":              "",
		"LYEVE_SETUP_MODE":        "",
		"LYEVE_PLUGINS":           "",
		"MIGRATIONS_PATH":         filepath.Join("..", "..", "migrations"),
		"STORAGE_LOCAL_PATH":      t.TempDir(),
	} {
		t.Setenv(k, v)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(runCtx, Options{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Error("the engine did not stop")
		}
	})

	base := "http://" + admin
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.NoError(t, err, "the engine exited during boot")
		default:
		}
		resp, err := http.Get(base + "/api/admin/setup")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 60*time.Second, 25*time.Millisecond, "the engine never served /api/admin/setup")

	resp, err := http.Post(base+"/api/admin/setup", "application/json", strings.NewReader(
		`{"email":"root@stored.test","password":"Stored-settings-Passw0rd!","setup_token":"stored-settings-setup-token-01"}`))
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
	var setup struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &setup))

	assert.Equal(t, int64(1800), tokenLifetime(t, setup.Token), "the token was signed with the boot default, not the stored lifetime")
}

// tokenLifetime reads exp minus iat from a JWT's claims.
func tokenLifetime(t *testing.T, token string) int64 {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "not a JWT")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims struct {
		Exp int64 `json:"exp"`
		Iat int64 `json:"iat"`
	}
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims.Exp - claims.Iat
}
