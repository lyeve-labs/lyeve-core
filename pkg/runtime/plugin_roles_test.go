package runtime

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// guardPlugin runs with no database and holds the login challenge role, which
// one plugin holds.
type guardPlugin struct{ name string }

func (p *guardPlugin) Name() string                           { return p.name }
func (p *guardPlugin) StatelessCapable() bool                 { return true }
func (p *guardPlugin) Start(context.Context, core.Host) error { return nil }
func (p *guardPlugin) Stop(context.Context) error             { return nil }
func (p *guardPlugin) CaptchaMiddleware(context.Context) func(http.Handler) http.Handler {
	return nil
}

// Two running plugins that implement a role one plugin holds stop the boot,
// and the error names the role and both plugins. The engine could wire the
// role from either one, and which one would depend on nothing an operator can
// see.
func TestRun_RefusesARoleTwoRunningPluginsClaim(t *testing.T) {
	// A build that links no licensing implementation starts every compiled
	// plugin, so any two names reach the role check.
	first, second := "first-guard", "second-guard"

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lyeve.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("api_keys: []\n"), 0o600))
	for k, v := range map[string]string{
		"LYEVE_MODE":              "stateless",
		"LYEVE_CONFIG":            cfgPath,
		"APP_ENV":                 "development",
		"API_LISTEN_ADDR":         freeAddr(t),
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
	for _, name := range []string{first, second} {
		p := &guardPlugin{name: name}
		plugin.RegisterPlugin(name, func() core.Plugin { return p })
		t.Cleanup(func() { plugin.UnregisterPlugin(name) })
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(ctx, Options{}) }()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorContains(t, err, "core.CaptchaMiddlewareProvider")
		require.ErrorContains(t, err, first)
		require.ErrorContains(t, err, second)
	case <-time.After(30 * time.Second):
		cancel()
		<-done
		t.Fatal("the engine booted with two plugins claiming one role")
	}
}
