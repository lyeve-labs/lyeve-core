package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// The license wire is what the admin console and every operator script read
// about the license: the entitlements body, what a renewal answers, the
// tenant features routes, the plugin status, the security controls and the
// OpenAPI document. TestLicenseWire_MatchesTheGolden boots the real engine
// and holds each of those responses to a file in testdata, so a change to the
// engine cannot change what a client reads.
//
// This package links no licensing implementation, so the engine runs on
// licensing.Open. A licensed variant boots with a license credential in the
// environment and is held to the golden of its unlicensed twin, because a
// build that verifies nothing must answer the same whatever credential an
// operator sets.
//
// Each variant boots in a child process of its own. The boot installs process
// state that a second boot in the same process would collide with, and the
// child's environment holds only what the variant sets, so nothing in the
// shell that runs the test reaches the configuration.

var (
	licenseWireChild  = flag.String("license-wire-child", "", "the license wire variant this process boots, set only on the child process")
	licenseWireOut    = flag.String("license-wire-out", "", "the file the license wire child writes its exchanges to")
	updateLicenseWire = flag.Bool("update-license-wire", false, "rewrite the license wire golden files from this run")
)

// wireVariant is one engine the golden boots.
type wireVariant struct {
	stateless bool
	licensed  bool
}

// golden is the file the variant's exchanges are held to. A licensed variant
// shares its unlicensed twin's, since nothing in this build reads the
// credential it adds.
func (v wireVariant) golden() string {
	name := "unlicensed"
	if v.stateless {
		name = "stateless"
	}
	return filepath.Join("testdata", "license_wire", name+".golden")
}

var licenseWireVariants = map[string]wireVariant{
	"unlicensed":         {},
	"licensed":           {licensed: true},
	"stateless":          {stateless: true},
	"stateless-licensed": {stateless: true, licensed: true},
}

// wireExchange is one request the child made and what came back.
type wireExchange struct {
	Name         string `json:"name"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	As           string `json:"as"`
	Status       int    `json:"status"`
	ContentType  string `json:"content_type"`
	CacheControl string `json:"cache_control"`
	Allow        string `json:"allow"`
	Body         string `json:"body"`
}

func TestLicenseWire_MatchesTheGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the engine")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}
	for _, name := range []string{"unlicensed", "licensed", "stateless", "stateless-licensed"} {
		t.Run(name, func(t *testing.T) {
			v := licenseWireVariants[name]
			dsn := ""
			if !v.stateless {
				_, dsn = testdb.PostgresWithDSN(t)
			}
			got := renderLicenseWire(runLicenseWireChild(t, name, dsn))
			golden := v.golden()
			if *updateLicenseWire && !v.licensed {
				require.NoError(t, os.MkdirAll(filepath.Dir(golden), 0o755))
				require.NoError(t, os.WriteFile(golden, []byte(got), 0o600))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err, "no golden for %s: run with -update-license-wire to write it", name)
			if got != string(want) {
				t.Errorf("the %s wire differs from %s:\n%s", name, golden, firstDifference(string(want), got))
			}
		})
	}
}

// runLicenseWireChild boots one variant in a child process and returns what
// it recorded.
func runLicenseWireChild(t *testing.T, variant, dsn string) []wireExchange {
	t.Helper()
	out := filepath.Join(t.TempDir(), "exchanges.json")
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestLicenseWire_ChildProcess$", "-test.count=1", "-test.timeout=5m",
		"-license-wire-child="+variant, "-license-wire-out="+out)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "DATABASE_URL=" + dsn}
	for _, k := range []string{"TMPDIR", "GOCOVERDIR", "GORACE", "GOTRACEBACK"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Run(); err != nil {
		t.Fatalf("the %s child failed: %v\n%s", variant, err, lastLines(logs.String(), 80))
	}
	b, err := os.ReadFile(out)
	require.NoError(t, err, "the %s child wrote nothing\n%s", variant, lastLines(logs.String(), 80))
	var exchanges []wireExchange
	require.NoError(t, json.Unmarshal(b, &exchanges))
	return exchanges
}

// TestLicenseWire_ChildProcess is the engine the golden reads. It runs only
// when TestLicenseWire_MatchesTheGolden starts it with a variant.
func TestLicenseWire_ChildProcess(t *testing.T) {
	if *licenseWireChild == "" {
		t.Skip("runs only as the child process of TestLicenseWire_MatchesTheGolden")
	}
	v, ok := licenseWireVariants[*licenseWireChild]
	require.True(t, ok, "unknown variant %q", *licenseWireChild)
	exchanges := v.run(t)
	b, err := json.Marshal(exchanges)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(*licenseWireOut, b, 0o600))
}

// wirePlugin stands in for a compiled plugin: one admin route that says whose
// it is and which tenant asked. A licensing implementation could start widgets
// on every install and gadgets only under a license, so the two sit on either
// side of that decision. This build links none, and both start.
type wirePlugin struct{ name string }

func (p *wirePlugin) Name() string                           { return p.name }
func (p *wirePlugin) Start(context.Context, core.Host) error { return nil }
func (p *wirePlugin) Stop(context.Context) error             { return nil }
func (p *wirePlugin) StatelessCapable() bool                 { return true }
func (p *wirePlugin) Routes() []plugin.RouteDecl {
	return []plugin.RouteDecl{{
		Method: http.MethodGet, Pattern: "/api/admin/wire/" + p.name, Group: plugin.GroupAdmin,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.JSON(w, http.StatusOK, map[string]string{"plugin": p.name, "tenant": core.TenantIDFromCtx(r.Context())})
		}),
	}}
}

const (
	wireSetupToken  = "license-wire-setup-token-0001"
	wirePassword    = "Wire-Golden-Pass-2026!"
	wireOperatorKey = "license-wire-operator-key-0001"
	// wireLicenseKey is a license credential nothing in this build can verify.
	wireLicenseKey = "license-wire-key-0001"
)

func (v wireVariant) run(t *testing.T) []wireExchange {
	for _, name := range []string{"widgets", "gadgets"} {
		p := &wirePlugin{name: name}
		plugin.RegisterPlugin(name, func() core.Plugin { return p })
	}

	state := t.TempDir()
	env := map[string]string{
		"APP_ENV":                  "development",
		"INSTANCE_ID":              "license-wire",
		"LYEVE_LICENSE_CACHE_DIR":  state,
		"LYEVE_LICENSE_KEY":        "",
		"LYEVE_LICENSE_SERVER_URL": "https://license.invalid",
		"LYEVE_PLUGINS":            "",
		"STORAGE_LOCAL_PATH":       t.TempDir(),
	}
	addr := freeAddr(t)
	if v.stateless {
		cfgPath := filepath.Join(state, "lyeve.yaml")
		require.NoError(t, os.WriteFile(cfgPath, []byte("api_keys:\n  - name: operator\n    sha256: "+sha256Hex(wireOperatorKey)+"\n    roles: [super_admin]\n"), 0o600))
		env["LYEVE_MODE"] = "stateless"
		env["LYEVE_CONFIG"] = cfgPath
		env["API_LISTEN_ADDR"] = addr
	} else {
		env["ADMIN_LISTEN_ADDR"] = addr
		env["API_LISTEN_ADDR"] = freeAddr(t)
		env["JWT_SECRET"] = "license-wire-jwt-secret-0123456789abcdef0123"
		env["ENCRYPTION_KEY"] = "license-wire-encryption-key-distinct-012345"
		env["JWT_KEY_PATH"] = filepath.Join(state, "jwt_key.json")
		env["LYEVE_SETUP_TOKEN"] = wireSetupToken
		env["MIGRATIONS_PATH"] = filepath.Join("..", "..", "migrations")
	}
	if v.licensed {
		env["LYEVE_LICENSE_KEY"] = wireLicenseKey
	}
	for k, val := range env {
		t.Setenv(k, val)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWithOptions(ctx, Options{}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Error("the engine did not stop")
		}
	})

	c := &wireClient{t: t, base: "http://" + addr, http: &http.Client{Timeout: 30 * time.Second}}
	ready := "/api/admin/setup"
	if v.stateless {
		ready = "/readyz"
	}
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			require.NoError(t, err, "the engine exited during boot")
		default:
		}
		resp, err := c.http.Get(c.base + ready)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 60*time.Second, 25*time.Millisecond, "the engine never served %s", ready)

	if v.stateless {
		c.creds = map[string]string{"operator key": wireOperatorKey}
		c.do("entitlements with the operator key", http.MethodGet, "/api/admin/entitlements", "operator key", "")
		c.do("entitlements with no credential", http.MethodGet, "/api/admin/entitlements", "", "")
		c.do("plugin status with the operator key", http.MethodGet, "/api/admin/plugins/status", "operator key", "")
		c.do("running plugins with the operator key", http.MethodGet, "/api/admin/plugins/running", "operator key", "")
		c.do("renew with the operator key", http.MethodPost, "/api/admin/license/renew", "operator key", `{"license_key":"x"}`)
		c.do("tenant features with the operator key", http.MethodGet, "/api/admin/tenant-features/default", "operator key", "")
		c.do("an ungated plugin's route", http.MethodGet, "/api/admin/wire/widgets", "operator key", "")
		c.do("a gated plugin's route", http.MethodGet, "/api/admin/wire/gadgets", "operator key", "")
		return c.exchanges
	}

	c.signIn()

	c.do("entitlements as a super admin", http.MethodGet, "/api/admin/entitlements", "super admin", "")
	c.do("entitlements as an admin", http.MethodGet, "/api/admin/entitlements", "admin", "")
	c.do("entitlements with no credential", http.MethodGet, "/api/admin/entitlements", "", "")
	c.doWith("entitlements naming another tenant", http.MethodGet, "/api/admin/entitlements", "super admin", "", map[string]string{"X-Tenant-ID": "acme"})
	c.do("renew with no credential", http.MethodPost, "/api/admin/license/renew", "", `{"license_key":"x"}`)
	c.do("renew as an admin", http.MethodPost, "/api/admin/license/renew", "admin", `{"license_key":"x"}`)
	c.do("renew with an empty key", http.MethodPost, "/api/admin/license/renew", "super admin", `{}`)
	c.do("renew with a body that is not JSON", http.MethodPost, "/api/admin/license/renew", "super admin", `{`)
	c.do("renew by GET", http.MethodGet, "/api/admin/license/renew", "super admin", "")
	c.do("an ungated plugin's route", http.MethodGet, "/api/admin/wire/widgets", "admin", "")
	c.do("a gated plugin's route", http.MethodGet, "/api/admin/wire/gadgets", "admin", "")
	c.do("tenant features read", http.MethodGet, "/api/admin/tenant-features/default", "super admin", "")
	c.do("tenant features write", http.MethodPut, "/api/admin/tenant-features/default", "super admin", `{"withheld":["gadgets","widgets","gadgets"]}`)
	c.do("plugin status", http.MethodGet, "/api/admin/plugins/status", "super admin", "")
	c.do("running plugins as an admin", http.MethodGet, "/api/admin/plugins/running", "admin", "")
	c.do("security controls", http.MethodGet, "/api/admin/security/controls", "super admin", "")
	c.do("openapi as a super admin", http.MethodGet, "/api/admin/openapi.json", "super admin", "")
	c.do("openapi as an admin", http.MethodGet, "/api/admin/openapi.json", "admin", "")
	return c.exchanges
}

// wireClient makes the requests the golden records.
type wireClient struct {
	t         *testing.T
	base      string
	http      *http.Client
	creds     map[string]string // who -> bearer token, or the API key in stateless mode
	exchanges []wireExchange
}

// signIn claims the first super admin through setup and signs in an admin
// the super admin creates, the way an operator's first minutes go.
func (c *wireClient) signIn() {
	c.t.Helper()
	var setup struct {
		Token string `json:"token"`
	}
	c.call(http.MethodPost, "/api/admin/setup", "", `{"email":"root@wire.test","password":"`+wirePassword+`","setup_token":"`+wireSetupToken+`"}`, nil, http.StatusCreated, &setup)
	c.creds = map[string]string{"super admin": setup.Token}
	c.call(http.MethodPost, "/api/admin/users", "super admin", `{"email":"admin@wire.test","password":"`+wirePassword+`","roles":["admin"]}`, nil, http.StatusCreated, nil)
	var login struct {
		Token string `json:"token"`
	}
	c.call(http.MethodPost, "/api/admin/auth/login", "", `{"email":"admin@wire.test","password":"`+wirePassword+`"}`, nil, http.StatusOK, &login)
	c.creds["admin"] = login.Token
}

func (c *wireClient) call(method, path, who, body string, header map[string]string, want int, into any) {
	c.t.Helper()
	status, _, raw := c.send(method, path, who, body, header)
	require.Equal(c.t, want, status, "%s %s: %s", method, path, raw)
	if into != nil {
		require.NoError(c.t, json.Unmarshal([]byte(raw), into))
	}
}

func (c *wireClient) do(name, method, path, who, body string) {
	c.t.Helper()
	c.doWith(name, method, path, who, body, nil)
}

func (c *wireClient) doWith(name, method, path, who, body string, header map[string]string) {
	c.t.Helper()
	status, h, raw := c.send(method, path, who, body, header)
	c.exchanges = append(c.exchanges, wireExchange{
		Name: name, Method: method, Path: path, As: who, Status: status,
		ContentType: h.Get("Content-Type"), CacheControl: h.Get("Cache-Control"), Allow: h.Get("Allow"),
		Body: raw,
	})
}

func (c *wireClient) send(method, path, who, body string, header map[string]string) (int, http.Header, string) {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	require.NoError(c.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if who != "" {
		cred, ok := c.creds[who]
		require.True(c.t, ok, "no credential for %q", who)
		if who == "operator key" {
			req.Header.Set("X-API-Key", cred)
		} else {
			req.Header.Set("Authorization", "Bearer "+cred)
		}
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	require.NoError(c.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	return resp.StatusCode, resp.Header, string(b)
}

// What varies between two runs of the same engine and is normalized away:
// timestamps, identifiers, request ids and the listener's port.
var (
	wireTime      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	wireUUID      = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	wireRequestID = regexp.MustCompile(`"request_id":"[^"]*"`)
	wirePort      = regexp.MustCompile(`127\.0\.0\.1:\d+`)
)

func normalizeWire(s string) string {
	s = wireTime.ReplaceAllString(s, "<time>")
	s = wireUUID.ReplaceAllString(s, "<uuid>")
	s = wireRequestID.ReplaceAllString(s, `"request_id":"<request-id>"`)
	return wirePort.ReplaceAllString(s, "127.0.0.1:<port>")
}

// renderLicenseWire writes the exchanges as the golden holds them. A JSON
// body is written with its object members sorted and every value exactly as
// it was sent: several responses are encoded from a map, whose member order
// the encoder does not fix, so the order carries no meaning a client could
// read. Whether the body ended with a newline is kept.
func renderLicenseWire(exchanges []wireExchange) string {
	var b strings.Builder
	for _, e := range exchanges {
		fmt.Fprintf(&b, "=== %s\n%s %s", e.Name, e.Method, e.Path)
		if e.As != "" {
			fmt.Fprintf(&b, " as %s", e.As)
		}
		fmt.Fprintf(&b, "\nstatus: %d\n", e.Status)
		for _, h := range [][2]string{{"Allow", e.Allow}, {"Cache-Control", e.CacheControl}, {"Content-Type", e.ContentType}} {
			if h[1] != "" {
				fmt.Fprintf(&b, "%s: %s\n", h[0], h[1])
			}
		}
		body := normalizeWire(e.Body)
		trimmed := strings.TrimSuffix(body, "\n")
		newline := "no newline"
		if trimmed != body {
			newline = "newline"
		}
		if canon, err := canonicalJSON([]byte(trimmed), ""); err == nil {
			fmt.Fprintf(&b, "body (json, %s):\n%s\n\n", newline, canon)
		} else {
			fmt.Fprintf(&b, "body (%s):\n%s\n\n", newline, trimmed)
		}
	}
	return b.String()
}

// canonicalJSON reindents raw with object members sorted by key. Strings,
// numbers and literals are copied as they were encoded, so an escaping or a
// number format that changed still shows.
func canonicalJSON(raw []byte, indent string) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", fmt.Errorf("empty")
	}
	switch raw[0] {
	case '{':
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return "", err
		}
		if len(members) == 0 {
			return "{}", nil
		}
		keys := make([]string, 0, len(members))
		for k := range members {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("{\n")
		for i, k := range keys {
			v, err := canonicalJSON(members[k], indent+"  ")
			if err != nil {
				return "", err
			}
			key, _ := json.Marshal(k)
			fmt.Fprintf(&b, "%s  %s: %s", indent, key, v)
			if i < len(keys)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "}")
		return b.String(), nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", err
		}
		if len(items) == 0 {
			return "[]", nil
		}
		var b strings.Builder
		b.WriteString("[\n")
		for i, item := range items {
			v, err := canonicalJSON(item, indent+"  ")
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "%s  %s", indent, v)
			if i < len(items)-1 {
				b.WriteString(",")
			}
			b.WriteString("\n")
		}
		b.WriteString(indent + "]")
		return b.String(), nil
	default:
		if !json.Valid(raw) {
			return "", fmt.Errorf("not JSON")
		}
		return string(raw), nil
	}
}

// firstDifference shows where two renderings part, with the lines around it.
func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		from := i - 6
		if from < 0 {
			from = 0
		}
		var ctx []string
		for j := from; j < i && j < len(g); j++ {
			ctx = append(ctx, "  "+g[j])
		}
		return fmt.Sprintf("line %d\n%s\n- %s\n+ %s", i+1, strings.Join(ctx, "\n"), wl, gl)
	}
	return "no difference"
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
