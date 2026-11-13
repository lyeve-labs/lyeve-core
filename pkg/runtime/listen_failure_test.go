package runtime

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// listenTakenChild marks the process that boots the engine below, and names
// the listen address it takes before the boot.
const listenTakenChild = "LYEVE_TEST_LISTEN_TAKEN_CHILD"

// An engine that cannot bind one of its listeners has to stop with an error,
// so its supervisor restarts it. Serving the other listener alone reads as
// healthy to anything but a request to the missing one. Each boot runs in a
// process of its own, because a process holds one engine's registries.
func TestRun_FailsWhenAListenAddressIsTaken(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the engine")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("postgres not selected")
	}
	if which := os.Getenv(listenTakenChild); which != "" {
		bootWithAddressTaken(t, which)
		return
	}
	for _, which := range []string{"ADMIN_LISTEN_ADDR", "API_LISTEN_ADDR"} {
		t.Run(which, func(t *testing.T) {
			cmd := exec.Command(os.Args[0],
				"-test.run=^TestRun_FailsWhenAListenAddressIsTaken$", "-test.count=1", "-test.timeout=5m")
			cmd.Env = append(os.Environ(), listenTakenChild+"="+which)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "the boot with %s taken:\n%s", which, out)
		})
	}
}

func bootWithAddressTaken(t *testing.T, which string) {
	_, dsn := testdb.PostgresWithDSN(t)

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })

	state := t.TempDir()
	env := map[string]string{
		"DATABASE_URL":            dsn,
		"DATABASE_REPLICA_URL":    "",
		"APP_ENV":                 "development",
		"ADMIN_LISTEN_ADDR":       freeAddr(t),
		"API_LISTEN_ADDR":         freeAddr(t),
		"JWT_SECRET":              "listen-taken-jwt-secret-0123456789abcdef012345",
		"ENCRYPTION_KEY":          "listen-taken-encryption-key-distinct-01234567",
		"JWT_KEY_PATH":            filepath.Join(state, "jwt_key.json"),
		"LYEVE_LICENSE_CACHE_DIR": state,
		"LYEVE_LICENSE_KEY":       "",
		"LYEVE_SETUP_TOKEN":       "listen-taken-setup-token-0001",
		"LYEVE_MODE":              "",
		"LYEVE_SETUP_MODE":        "",
		"LYEVE_PLUGINS":           "",
		"MIGRATIONS_PATH":         filepath.Join("..", "..", "migrations"),
		"STORAGE_LOCAL_PATH":      t.TempDir(),
	}
	env[which] = taken.Addr().String()
	for k, v := range env {
		t.Setenv(k, v)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(ctx, Options{}) }()

	select {
	case err := <-done:
		require.Error(t, err, "the engine stopped cleanly with %s taken", which)
		assert.Contains(t, err.Error(), taken.Addr().String(), "the error names the address it could not bind")
	case <-time.After(60 * time.Second):
		cancel()
		<-done
		t.Fatalf("the engine kept running with %s taken", which)
	}
	cancel()
}
