package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// relaySection is the configuration section the relay reads its resources
// from. A plugin registers its section from init, because the settings loader
// reads the file before any plugin starts, and the relay does the same.
const relaySection = "webhooks"

func init() { core.RegisterResourceSection(relaySection) }

// relayPlugin stands in for a plugin that runs without a database. Its store
// is whatever the host's querier answers, so a query it makes shows what a
// plugin sees on a stateless engine.
type relayPlugin struct {
	name     string
	capable  bool
	queryErr error
	declared int
}

func (p *relayPlugin) Name() string           { return p.name }
func (p *relayPlugin) StatelessCapable() bool { return p.capable }
func (p *relayPlugin) Stop(context.Context) error {
	return nil
}
func (p *relayPlugin) Start(ctx context.Context, host core.Host) error {
	var n int
	row, _ := host.Querier(ctx).QueryRow(ctx, "SELECT 1")
	p.queryErr = row.Scan(&n)
	if !core.IsStateless(host.Config()) {
		return errors.New("the host does not tell a plugin it is stateless")
	}
	docs, err := core.DeclaredResources(host, relaySection)
	if err != nil {
		return err
	}
	p.declared = len(docs)
	if lock := host.DistLock("relay"); lock != nil {
		return errors.New("a stateless host handed out a database lock")
	}
	return nil
}
func (p *relayPlugin) Routes() []plugin.RouteDecl {
	tenant := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, core.TenantIDFromCtx(r.Context()))
	})
	return []plugin.RouteDecl{
		{Method: http.MethodGet, Pattern: "/api/v1/" + p.name + "/tenant", Handler: tenant, Group: plugin.GroupAuth},
		{Method: http.MethodGet, Pattern: "/api/v1/" + p.name + "/public", Handler: tenant, Group: plugin.GroupPublic},
		{Method: http.MethodGet, Pattern: "/api/v1/" + p.name + "-reports/tenant", Handler: tenant, Group: plugin.GroupAuth},
	}
}

// registerForTest compiles p in for the test. It declares what a plugin
// reading the database and serving routes declares, because a build that hands
// the engine no capability table grants a plugin only what it declared.
func registerForTest(t *testing.T, p *relayPlugin) {
	t.Helper()
	plugin.RegisterPluginWithCaps(p.name, func() core.Plugin { return p }, core.CapDBWrite|core.CapRawDB|core.CapRoutes)
	t.Cleanup(func() { plugin.UnregisterPlugin(p.name) })
}

func TestRunStateless_BootsWithNoDatabase(t *testing.T) {
	const operatorKey = "operator-key-for-the-stateless-test"
	const readerKey = "reader-key-for-the-stateless-test"
	const expiredKey = "expired-key-for-the-stateless-test"
	const otherScopeKey = "other-scope-key-for-the-stateless-test"

	// A build that links no licensing implementation starts every compiled
	// plugin, so only the stateless capability decides which fixture starts.
	relayName, dbName := "relay", "ledger"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lyeve.yaml"), []byte(`
api_keys:
  - name: operator
    sha256: `+sha256Hex(operatorKey)+`
    roles: [super_admin]
  - name: reader
    sha256: `+sha256Hex(readerKey)+`
    scopes: ["`+relayName+`:read"]
  - name: other-scope
    sha256: `+sha256Hex(otherScopeKey)+`
    scopes: ["flows:read"]
  - name: expired
    sha256: `+sha256Hex(expiredKey)+`
    roles: [super_admin]
    expires_at: 2020-01-01T00:00:00Z
webhooks:
  - name: downstream
    url: https://downstream.example.com/hook
`), 0o600))

	addr := freeAddr(t)
	for k, v := range map[string]string{
		"LYEVE_MODE":              "stateless",
		"LYEVE_CONFIG":            filepath.Join(dir, "lyeve.yaml"),
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

	relay := &relayPlugin{name: relayName, capable: true}
	needsDB := &relayPlugin{name: dbName}
	registerForTest(t, relay)
	registerForTest(t, needsDB)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(ctx, Options{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err, "a stateless engine stops cleanly when its context ends")
		case <-time.After(10 * time.Second):
			t.Error("the stateless engine did not stop")
		}
	})

	base := "http://" + addr
	getAs := func(path, key, tenant string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		require.NoError(t, err)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		if tenant != "" {
			req.Header.Set("X-Tenant-ID", tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	get := func(path, key string) (int, string) {
		t.Helper()
		return getAs(path, key, "")
	}

	require.Eventually(t, func() bool {
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "the stateless engine never became ready")

	t.Run("the operator key reads the mode, and no hash leaves the engine", func(t *testing.T) {
		code, body := get("/api/admin/mode", operatorKey)
		require.Equal(t, http.StatusOK, code, body)
		assert.Contains(t, body, `"mode":"stateless"`)
		assert.Contains(t, body, `"metered":false`)
		assert.Contains(t, body, `"database":false`)
		assert.NotContains(t, body, sha256Hex(operatorKey))
	})
	t.Run("a request with no key is refused", func(t *testing.T) {
		code, _ := get("/api/admin/mode", "")
		assert.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("a scoped key without a role is refused the admin routes", func(t *testing.T) {
		code, _ := get("/api/admin/mode", readerKey)
		assert.Equal(t, http.StatusForbidden, code)
	})
	t.Run("an expired key is refused like an unknown one", func(t *testing.T) {
		code, _ := get("/api/admin/mode", expiredKey)
		assert.Equal(t, http.StatusUnauthorized, code)
		code, _ = get("/api/admin/mode", "a-key-nobody-declared")
		assert.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("a capable plugin serves its routes as the default tenant", func(t *testing.T) {
		code, body := get("/api/v1/"+relayName+"/tenant", readerKey)
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, core.DefaultTenantSlug, body)
		code, _ = get("/api/v1/"+relayName+"/tenant", "")
		assert.Equal(t, http.StatusUnauthorized, code)
		code, body = get("/api/v1/"+relayName+"/public", "")
		assert.Equal(t, http.StatusOK, code, body)
	})
	t.Run("a scoped key is held to its scopes on an authenticated plugin route", func(t *testing.T) {
		code, _ := get("/api/v1/"+relayName+"-reports/tenant", readerKey)
		assert.Equal(t, http.StatusForbidden, code, relayName+":read does not reach the "+relayName+"-reports resource")
		code, _ = get("/api/v1/"+relayName+"-reports/tenant", operatorKey)
		assert.Equal(t, http.StatusOK, code, "a role is not narrowed by a scope")
	})
	t.Run("a super_admin key cannot name a second tenant", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, base+"/api/v1/"+relayName+"/tenant", nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", operatorKey)
		req.Header.Set("X-Tenant-ID", "acme")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
	t.Run("a plugin that needs a database is not started and serves nothing", func(t *testing.T) {
		code, _ := get("/api/v1/"+dbName+"/public", "")
		assert.Equal(t, http.StatusNotFound, code)
		code, body := get("/api/admin/plugins/status", operatorKey)
		require.Equal(t, http.StatusOK, code, body)
		assert.Contains(t, body, "needs a database; the engine is running in stateless mode")
	})
	t.Run("a key reads which plugins run, by name alone", func(t *testing.T) {
		code, body := get("/api/admin/plugins/running", operatorKey)
		require.Equal(t, http.StatusOK, code, body)
		var running struct {
			Plugins  []string `json:"plugins"`
			Withheld []string `json:"withheld"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &running))
		assert.Contains(t, running.Plugins, relayName)
		assert.NotContains(t, running.Plugins, dbName, "a plugin the stateless engine did not start serves nobody")
		assert.NotNil(t, running.Withheld)
		assert.Empty(t, running.Withheld, "one tenant, and nothing withheld from it")
		code, _ = get("/api/admin/plugins/running", "")
		assert.Equal(t, http.StatusUnauthorized, code)
	})
	t.Run("a query reports the mode instead of reaching a pool", func(t *testing.T) {
		assert.ErrorIs(t, relay.queryErr, core.ErrNoDatabase)
	})
	t.Run("a plugin reads its declared resources through the host", func(t *testing.T) {
		assert.Equal(t, 1, relay.declared)
	})
	t.Run("there is no login", func(t *testing.T) {
		code, _ := get("/api/admin/auth/me", operatorKey)
		assert.Equal(t, http.StatusNotFound, code)
	})
}

func TestDeclaredKeyLookup_ActsUnderAStableID(t *testing.T) {
	keys := []config.DeclaredAPIKey{{Name: "relay", SHA256: sha256Hex("k"), Scopes: []string{"flows:write"}}}
	lookup := declaredKeyLookup(keys, time.Now)

	first, err := lookup(context.Background(), sha256Hex("k"))
	require.NoError(t, err)
	require.NotNil(t, first)
	again, err := declaredKeyLookup(keys, time.Now)(context.Background(), sha256Hex("k"))
	require.NoError(t, err)
	assert.Equal(t, first.UserID, again.UserID, "the same name is the same id after a restart and on every replica")
	assert.Equal(t, first.UserID, first.APIKeyID)
	assert.True(t, first.IsAPIKey)
	assert.Equal(t, "apikey:relay", first.Email)
}
