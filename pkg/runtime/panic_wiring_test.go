package runtime

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// panicHolderPlugin holds the panic reporter role and serves one public route
// that panics.
type panicHolderPlugin struct {
	reports chan core.RecoveredPanic
}

func (p *panicHolderPlugin) Name() string                           { return "panic-holder" }
func (p *panicHolderPlugin) StatelessCapable() bool                 { return true }
func (p *panicHolderPlugin) Start(context.Context, core.Host) error { return nil }
func (p *panicHolderPlugin) Stop(context.Context) error             { return nil }
func (p *panicHolderPlugin) Routes() []plugin.RouteDecl {
	boom := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("route boom") })
	return []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/v1/panic-holder/boom", Handler: boom, Group: plugin.GroupPublic}}
}

func (p *panicHolderPlugin) ReportPanic(rp core.RecoveredPanic) {
	select {
	case p.reports <- rp:
	default:
	}
}

// bootWithPanicHolder registers the holder, boots the engine with the
// environment already set, and stops it when the test ends.
func bootWithPanicHolder(t *testing.T) *panicHolderPlugin {
	t.Helper()
	p := &panicHolderPlugin{reports: make(chan core.RecoveredPanic, 16)}
	plugin.RegisterPluginWithCaps(p.Name(), func() core.Plugin { return p }, core.CapRoutes)
	t.Cleanup(func() { plugin.UnregisterPlugin(p.Name()) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(ctx, Options{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(90 * time.Second):
			t.Error("the engine did not stop")
		}
	})
	return p
}

// awaitReport returns the first report that matches, or fails the test.
func awaitReport(t *testing.T, p *panicHolderPlugin, match func(core.RecoveredPanic) bool) core.RecoveredPanic {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case rp := <-p.reports:
			if match(rp) {
				return rp
			}
		case <-timeout:
			t.Fatal("the engine recovered the panic but no plugin holding the role was told")
		}
	}
}

func awaitStatus(t *testing.T, url string, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == want
	}, 30*time.Second, 50*time.Millisecond, "%s never answered %d", url, want)
}

// The runtime installs the activator as the engine's panic reporter. Every
// other test installs its own, so this is the one that fails when the engine
// stops handing its panics to the plugins holding the role.
func TestRunStateless_HandsRecoveredPanicsToTheRoleHolders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lyeve.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("api_keys: []\n"), 0o600))
	addr := freeAddr(t)
	for k, v := range map[string]string{
		"LYEVE_MODE":              "stateless",
		"LYEVE_CONFIG":            cfgPath,
		"APP_ENV":                 "development",
		"API_LISTEN_ADDR":         addr,
		"LYEVE_LICENSE_KEY":       "",
		"LYEVE_LICENSE_CACHE_DIR": t.TempDir(),
		"DATABASE_URL":            "",
		"DATABASE_REPLICA_URL":    "",
		"JWT_SECRET":              "",
		"ENCRYPTION_KEY":          "",
		"LYEVE_SETUP_MODE":        "",
		"MULTI_TENANT":            "",
		"LYEVE_PLUGINS":           "",
	} {
		t.Setenv(k, v)
	}
	p := bootWithPanicHolder(t)
	awaitStatus(t, "http://"+addr+"/readyz", http.StatusOK)

	t.Run("a route's panic names its pattern", func(t *testing.T) {
		resp, err := http.Get("http://" + addr + "/api/v1/panic-holder/boom")
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		rp := awaitReport(t, p, func(rp core.RecoveredPanic) bool { return rp.Value == "route boom" })
		assert.Equal(t, "/api/v1/panic-holder/boom", rp.Route)
		assert.Equal(t, http.MethodGet, rp.Method)
	})
	t.Run("a goroutine's panic names the goroutine", func(t *testing.T) {
		engine.GoSafe("stateless-wiring", func() { panic("goroutine boom") })
		awaitReport(t, p, func(rp core.RecoveredPanic) bool { return rp.Goroutine == "stateless-wiring" })
	})
}

// panicWiringChild marks the process that runs the database boot below.
const panicWiringChild = "LYEVE_TEST_PANIC_WIRING_CHILD"

func TestRun_HandsRecoveredPanicsToTheRoleHolders(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("postgres not selected")
	}
	// A database boot registers process-wide providers and metrics that a
	// second boot in one process registers again, and another test in this
	// binary boots first, so this boot runs in a process of its own.
	if os.Getenv(panicWiringChild) == "" {
		cmd := exec.Command(os.Args[0],
			"-test.run=^TestRun_HandsRecoveredPanicsToTheRoleHolders$", "-test.count=1", "-test.timeout=5m")
		cmd.Env = append(os.Environ(), panicWiringChild+"=1")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "the database boot failed in its own process:\n%s", out)
		return
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
		"JWT_SECRET":              "panic-wiring-jwt-secret-0123456789abcdef0123456",
		"ENCRYPTION_KEY":          "panic-wiring-encryption-key-distinct-012345678",
		"JWT_KEY_PATH":            filepath.Join(state, "jwt_key.json"),
		"LYEVE_LICENSE_CACHE_DIR": state,
		"LYEVE_LICENSE_KEY":       "",
		"LYEVE_SETUP_TOKEN":       "panic-wiring-setup-token-0001",
		"LYEVE_MODE":              "",
		"LYEVE_SETUP_MODE":        "",
		"LYEVE_PLUGINS":           "",
		"MIGRATIONS_PATH":         filepath.Join("..", "..", "migrations"),
		"STORAGE_LOCAL_PATH":      t.TempDir(),
	} {
		t.Setenv(k, v)
	}
	p := bootWithPanicHolder(t)
	awaitStatus(t, "http://"+admin+"/api/admin/setup", http.StatusOK)

	engine.GoSafe("database-wiring", func() { panic("goroutine boom") })
	awaitReport(t, p, func(rp core.RecoveredPanic) bool { return rp.Goroutine == "database-wiring" })
}
