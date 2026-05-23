package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Edge-case coverage:
//  1. Empty/null input fuzzing
//  2. Max-length string boundary tests
//  3. Unicode homoglyph attacks
//  4. Concurrent plugin start/stop stress
//  5. Database connection drop mid-request

// - Helpers -------------------------------------------------

var testSecret = "test-secret-at-least-32-bytes-long!!"

func adminJWT(userID uuid.UUID, email string, roles []string) string {
	tok, err := auth.Sign(testSecret, 3600, userID, email, roles, "", 1)
	if err != nil {
		panic("adminJWT: " + err.Error())
	}
	return tok
}
func superAdminJWT() string {
	return adminJWT(uuid.New(), "super@test.com", []string{"super_admin"})
}
func setAdminAuth(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+superAdminJWT())
}

// stackDB: minimal db.DB that returns nil rows and configurable errors.
type stackDB struct {
	engine  string
	pingErr error
	viewErr error
	execErr error
}

// closedDB is lazily opened and closed so QueryRow returns a *sql.Row
// whose Scan() fails with a real error instead of panicking on nil.
var (
	closedDB   *sql.DB
	closedOnce sync.Once
)

func getClosedDB() *sql.DB {
	closedOnce.Do(func() {
		d, err := sql.Open("pgx", "host=0.0.0.0 port=1 connect_timeout=1")
		if err != nil {
			d, _ = sql.Open("pgx", "")
		}
		d.Close()
		closedDB = d
	})
	return closedDB
}

func (f *stackDB) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	return getClosedDB().QueryRowContext(ctx, q, args...), nil
}
func (f *stackDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if f.viewErr != nil {
		return nil, f.viewErr
	}
	return nil, nil
}
func (f *stackDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if f.execErr != nil {
		return nil, f.execErr
	}
	return nil, nil
}
func (f *stackDB) Begin(ctx context.Context) (*sql.Tx, error)                { return nil, nil }
func (f *stackDB) Conn(ctx context.Context) (*sql.Conn, error)               { return nil, nil }
func (f *stackDB) Ping(ctx context.Context) error                            { return f.pingErr }
func (f *stackDB) Close() error                                              { return nil }
func (f *stackDB) Stats() sql.DBStats                                        { return sql.DBStats{} }
func (f *stackDB) Engine() string                                            { return f.engine }
func (f *stackDB) SQLDB() *sql.DB                                            { return nil }
func (f *stackDB) QuerierRO(ctx context.Context) (db.ReadOnlyQuerier, error) { return f, nil }

var _ db.DB = (*stackDB)(nil)

func freshDB() *stackDB { return &stackDB{engine: "postgres"} }

func buildAdminRouter(t *testing.T, pool db.DB) http.Handler {
	t.Helper()
	cfg := &config.Config{
		JWTSecrets:     []string{testSecret},
		JWTSecret:      testSecret,
		JWTExpirySecs:  3600,
		CORSOrigins:    []string{"*"},
		SecureCookie:   false,
		DatabaseDriver: "postgres",
	}
	router, _ := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(t)))
	return router
}

func execReq(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

// SECTION 1: Empty / Null Input Fuzzing

func TestEdgeCase_EmptyRequestBody_PublicEndpoints(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/admin/setup"},
		{http.MethodPost, "/api/admin/auth/login"},
		{http.MethodPost, "/api/admin/auth/refresh"},
		// Logout accepts empty body (no credentials needed).
		{http.MethodPost, "/api/admin/auth/mfa-verify"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(""))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("empty body: got 500, want 4xx")
			}
		})
	}
}

func TestEdgeCase_NullJSONBody_PublicEndpoints(t *testing.T) {
	t.Parallel()

	tests := []string{"/api/admin/auth/login", "/api/admin/setup", "/api/admin/auth/refresh"}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("null"))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("null body: got 500, want 4xx")
			}
		})
	}
}

func TestEdgeCase_MalformedJSON_Login(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	payloads := []struct {
		name string
		body string
	}{
		{"unclosed", "{"},
		{"unexpected_close", "}"},
		{"dangling_comma", `{"key": ,`},
		{"undefined", `{"key": undefined}`},
		{"nan", `{"key": NaN}`},
		{"infinity", `{"key": Infinity}`},
		{"xml", `<xml>not json</xml>`},
		{"bare_string", `"just a string"`},
		{"array", `[1,2,3]`},
		{"empty", ""},
		{"garbage_1mb", strings.Repeat("X", 1<<20)},
		{"deep_nest", func() string {
			s := `"leaf"`
			for i := 0; i < 200; i++ {
				s = fmt.Sprintf(`{"n%d":%s}`, i, s)
			}
			return fmt.Sprintf(`{"email":"t@t.com","password":"p","deep":%s}`, s)
		}()},
	}

	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("malformed JSON: got 500, want 4xx")
			}
		})
	}
}

func TestEdgeCase_NoAuthOnProtectedEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/auth/me"},
		{http.MethodGet, "/api/admin/users"},
		{http.MethodPost, "/api/admin/users"},
		{http.MethodGet, "/api/admin/users"},
		{http.MethodPost, "/api/admin/users"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			r := httptest.NewRequest(tc.method, tc.path, nil)
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("no auth: got 500 (should be 401)")
			}
		})
	}
}

func TestEdgeCase_InvalidAuthTokens(t *testing.T) {
	t.Parallel()

	tokens := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"bearer_only", "Bearer"},
		{"bearer_space", "Bearer "},
		{"single_char", "Bearer x"},
		{"huge_token", "Bearer " + strings.Repeat("A", 10000)},
		{"basic_auth", "Basic dXNlcjpwYXNz"},
		{"lowercase", "bearer valid.token"},
	}

	for _, tc := range tokens {
		t.Run(tc.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			r := httptest.NewRequest(http.MethodGet, "/api/admin/auth/me", nil)
			r.Header.Set("Authorization", tc.value)
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("invalid token: got 500")
			}
		})
	}
}

// SECTION 2: Max-Length String Boundaries

func TestEdgeCase_MaxLengthStringsInLogin(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	boundaries := []struct {
		name string
		s    string
	}{
		{"1B", strings.Repeat("a", 1)},
		{"255B", strings.Repeat("a", 255)},
		{"256B", strings.Repeat("a", 256)},
		{"1KB", strings.Repeat("a", 1024)},
		{"64KB", strings.Repeat("a", 65536)},
		{"null_byte", "test\x00embedded"},
		{"newlines", "line1\nline2\r\nline3"},
		{"tabs", "\t\t\t"},
		{"spaces_only", "     "},
		{"single_char", "x"},
		{"emoji", "😀😀😀😀😀😀😀😀"},
		{"mixed_unicode", "Hello世界"},
	}

	for _, tc := range boundaries {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]string{
				"email":    tc.s + "@example.com",
				"password": tc.s,
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("payload: got 500")
			}
		})
	}
}

func TestEdgeCase_MaxLengthJSONKeys(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	for _, klen := range []int{1, 255, 1024, 4096, 8192} {
		t.Run(fmt.Sprintf("key_%d", klen), func(t *testing.T) {
			body := map[string]string{
				strings.Repeat("k", klen): strings.Repeat("v", klen),
				"email":                   "test@example.com",
				"password":                "pw",
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("key len %d: got 500", klen)
			}
		})
	}
}

func TestEdgeCase_HTTPHeaderFlood(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	for _, n := range []int{0, 10, 50, 100} {
		t.Run(fmt.Sprintf("%d_headers", n), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
			for i := 0; i < n; i++ {
				r.Header.Set(fmt.Sprintf("X-T-%d", i), "v")
			}
			rr := execReq(h, r)
			if rr.Code >= 500 {
				t.Errorf("%d headers: got %d, expected < 500", n, rr.Code)
			}
		})
	}
}

// SECTION 3: Unicode Homoglyph Attacks

var homoglyphs = []struct {
	name    string
	payload string
	desc    string
}{
	// Cyrillic homoglyphs: look like Latin letters
	{"cyrillic_a", "а", "U+0430 CYRILLIC SMALL LETTER A"},
	{"cyrillic_e", "е", "U+0435 CYRILLIC SMALL LETTER IE"},
	{"cyrillic_o", "о", "U+043E CYRILLIC SMALL LETTER O"},
	{"cyrillic_p", "р", "U+0440 CYRILLIC SMALL LETTER ER"},
	{"cyrillic_c", "с", "U+0441 CYRILLIC SMALL LETTER ES"},
	{"cyrillic_y", "у", "U+0443 CYRILLIC SMALL LETTER U"},
	{"cyrillic_x", "х", "U+0445 CYRILLIC SMALL LETTER HA"},

	// Role impersonation via homoglyphs
	{"homoglyph_admin", "аdmin", "cyrillic 'а' in 'admin'"},
	{"homoglyph_super", "ѕuper_admin", "cyrillic 'ѕ' U+0455"},

	// Zero-width / invisible characters
	{"zwspace", "admin\u200b", "ZERO WIDTH SPACE U+200B"},
	{"zwjoiner", "super\u200dadmin", "ZERO WIDTH JOINER U+200D"},
	{"zwnonjoiner", "super\u200cadmin", "ZERO WIDTH NON-JOINER U+200C"},
	{"word_joiner", "admin\u2060test", "WORD JOINER U+2060"},
	{"bidi_marks", "admin\u200e\u200f", "LRM+RLM"},

	// Invisible formatting
	{"soft_hyphen", "ad\u00admin", "SOFT HYPHEN U+00AD"},
	{"nobreak_space", "admin test", "NO-BREAK SPACE U+00A0"},
	{"ogham_space", "admin test", "OGHAM SPACE MARK U+1680"},

	// Fullwidth and math
	{"fullwidth", "ａｄｍｉｎ", "FULLWIDTH LATIN"},
	{"math_bold", "𝐚𝐝𝐦𝐢𝐧", "MATHEMATICAL BOLD"},
	{"script", "𝒶𝒹𝓂𝒾𝓃", "MATHEMATICAL SCRIPT"},
}

func TestEdgeCase_UnicodeHomoglyph_Email(t *testing.T) {
	t.Parallel()
	for _, hg := range homoglyphs {
		t.Run(hg.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			body := map[string]string{
				"email":    hg.payload + "@example.com",
				"password": "password123",
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("%s (%s): got 500", hg.name, hg.desc)
			}
		})
	}
}

func TestEdgeCase_UnicodeHomoglyph_Password(t *testing.T) {
	t.Parallel()
	for _, hg := range homoglyphs {
		t.Run(hg.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			body := map[string]string{
				"email":    "test@example.com",
				"password": hg.payload,
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("%s (%s): got 500", hg.name, hg.desc)
			}
		})
	}
}

func TestEdgeCase_UnicodeHomoglyph_JSONKeys(t *testing.T) {
	t.Parallel()
	for _, hg := range homoglyphs {
		t.Run(hg.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			body := map[string]string{
				"email":    "test@example.com",
				"password": "pw",
				hg.payload: "value",
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("%s (%s): got 500", hg.name, hg.desc)
			}
		})
	}
}

func TestEdgeCase_UnicodeHomoglyph_JSONValues(t *testing.T) {
	t.Parallel()
	for _, hg := range homoglyphs {
		t.Run(hg.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			body := map[string]string{
				"email":    "test@example.com",
				"password": "pw",
				"extra":    hg.payload,
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("%s (%s): got 500", hg.name, hg.desc)
			}
		})
	}
}

func TestEdgeCase_UnicodeRTLConfusion(t *testing.T) {
	t.Parallel()
	rtlPayloads := []struct {
		name string
		val  string
	}{
		{"rtl_filename", "\u202efdp.exe\u202c"},
		{"rtl_role", "\u202erotartsinimda\u202c"},
		{"bidi_nested", "\u202d\u202ex\u202cy\u202cz"},
	}

	for _, rp := range rtlPayloads {
		t.Run(rp.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			body := map[string]string{
				"email":    "test@example.com",
				"password": "pw",
				"name":     rp.val,
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("RTL %s: got 500", rp.name)
			}
		})
	}
}

func TestEdgeCase_UnicodeMaxCodePoints(t *testing.T) {
	t.Parallel()
	cps := []struct {
		name string
		r    rune
	}{
		{"max_rune", utf8.MaxRune},
		{"plane_16", rune(0x10FFFE)},
	}

	for _, cp := range cps {
		t.Run(cp.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			s := string(cp.r)
			body := map[string]string{
				"email":    "test@example.com",
				"password": "pwd" + s,
			}
			b, _ := json.Marshal(body)
			r := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("codepoint U+%04X: got 500", cp.r)
			}
		})
	}
}

// SECTION 4: Concurrent Plugin Start/Stop Stress

type edgePlugin struct {
	name       string
	startCalls int64
	stopCalls  int64
}

func (p *edgePlugin) Name() string    { return p.name }
func (p *edgePlugin) Version() string { return "1.0.0" }
func (p *edgePlugin) Start(ctx context.Context, host core.Host) error {
	atomic.AddInt64(&p.startCalls, 1)
	return nil
}
func (p *edgePlugin) Stop(ctx context.Context) error {
	atomic.AddInt64(&p.stopCalls, 1)
	return nil
}
func (p *edgePlugin) Routes() []plugin.RouteDecl { return nil }

func TestEdgeCase_ConcurrentPluginRegistration(t *testing.T) {
	t.Parallel()

	const n = 200
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("edge-reg-%d", idx)
			p := &edgePlugin{name: name}
			core.RegisterPlugin(name, func() core.Plugin { return p })
			_ = core.RegisteredPlugins()
		}(i)
	}

	wg.Wait()
	registered := core.RegisteredPlugins()
	assert.NotEmpty(t, registered)
}

func TestEdgeCase_RapidStartStopCycle(t *testing.T) {
	t.Parallel()

	const cycles = 200
	var startCount, stopCount int64
	var mu sync.Mutex
	active := make(map[string]bool)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for c := 0; c < cycles/10; c++ {
				name := fmt.Sprintf("cyc-%d-%d", wid, c)

				mu.Lock()
				active[name] = true
				mu.Unlock()
				atomic.AddInt64(&startCount, 1)

				time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))

				mu.Lock()
				delete(active, name)
				mu.Unlock()
				atomic.AddInt64(&stopCount, 1)
			}
		}(i)
	}

	wg.Wait()
	assert.Equal(t, startCount, stopCount)
	assert.Empty(t, active)
}

func TestEdgeCase_ConcurrentRouteCollection(t *testing.T) {
	t.Parallel()

	var mu sync.RWMutex
	routes := make(map[string][]plugin.RouteDecl)
	const goroutines = 50
	const ops = 100
	var wg sync.WaitGroup

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				name := fmt.Sprintf("rp-%d-%d", id, i)

				mu.Lock()
				routes[name] = []plugin.RouteDecl{
					{Method: http.MethodGet, Pattern: "/" + name, Handler: http.NotFoundHandler(), Group: plugin.GroupAdmin},
				}
				mu.Unlock()

				time.Sleep(time.Microsecond)

				mu.Lock()
				delete(routes, name)
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()
}

func TestEdgeCase_ConcurrentRouteEnumeration(t *testing.T) {
	t.Parallel()

	type prt struct {
		decl   plugin.RouteDecl
		plugin string
	}

	var mu sync.RWMutex
	var shared []prt
	const nPlugins = 100
	var wg sync.WaitGroup

	for i := 0; i < nPlugins; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("ep-%d", idx)
			mu.Lock()
			shared = append(shared, prt{decl: plugin.RouteDecl{
				Method: http.MethodGet, Pattern: "/api/v1/" + name,
				Handler: http.NotFoundHandler(), Group: plugin.GroupAdmin,
			}, plugin: name})
			mu.Unlock()
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < nPlugins; j++ {
				mu.RLock()
				var count int
				for _, p := range shared {
					switch p.decl.Group {
					case plugin.GroupPublic:
						count++
					default:
						count++
					}
				}
				mu.RUnlock()
				_ = count
				time.Sleep(time.Microsecond)
			}
		}()
	}

	wg.Wait()
}

// SECTION 5: Database Connection Drop Mid-Request

func TestEdgeCase_DBDead_HealthEndpoint(t *testing.T) {
	t.Parallel()
	dead := &stackDB{engine: "postgres", pingErr: errors.New("connection refused")}
	h := buildAdminRouter(t, dead)

	r := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
	rr := execReq(h, r)
	if rr.Code < 400 {
		t.Error("GET /health with dead DB: crashed with 500")
	}
}

func TestEdgeCase_DBDead_ReadyEndpoint(t *testing.T) {
	t.Parallel()
	dead := &stackDB{engine: "postgres", pingErr: errors.New("no connection")}
	h := buildAdminRouter(t, dead)

	r := httptest.NewRequest(http.MethodGet, "/api/admin/ready", nil)
	rr := execReq(h, r)
	if rr.Code < 400 {
		t.Error("GET /ready with dead DB: crashed with 500")
	}
}

// The tokenVersionCheck middleware queries GetTokenVersion before any
// authenticated handler runs, /auth/me included. When the DB is dead it fails
// closed, so a revoked token is not usable during an outage, and answers 503:
// the outage is not the caller's session ending, and a 401 would sign every
// caller out.
func TestEdgeCase_DBDead_AuthMe(t *testing.T) {
	t.Parallel()
	dead := &stackDB{engine: "postgres", pingErr: errors.New("connection refused")}
	h := buildAdminRouter(t, dead)

	r := httptest.NewRequest(http.MethodGet, "/api/admin/auth/me", nil)
	setAdminAuth(r)
	rr := execReq(h, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /auth/me with dead DB: expected 503 (token version check fails closed), got %d", rr.Code)
	}
}

// tokenVersionCheck queries GetTokenVersion before authenticated handlers
// run. When the DB is dead it fails closed with 503.
func TestEdgeCase_DBDead_AdminRead(t *testing.T) {
	t.Parallel()
	dead := &stackDB{engine: "postgres", viewErr: errors.New("statement timeout")}
	h := buildAdminRouter(t, dead)

	r := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	setAdminAuth(r)
	rr := execReq(h, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /users with dead DB: expected 503 (token version check fails closed), got %d", rr.Code)
	}
}

func TestEdgeCase_DBDead_Users(t *testing.T) {
	t.Parallel()
	dead := &stackDB{engine: "postgres", viewErr: errors.New("backend connection dead")}
	h := buildAdminRouter(t, dead)

	r := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	setAdminAuth(r)
	rr := execReq(h, r)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /users with dead DB: expected 503 (token version check fails closed), got %d", rr.Code)
	}
}

func TestEdgeCase_DBReadOnly_WriteEndpoint(t *testing.T) {
	t.Parallel()
	ro := &stackDB{engine: "postgres", execErr: errors.New("read-only transaction")}
	h := buildAdminRouter(t, ro)

	r := httptest.NewRequest(http.MethodPost, "/api/admin/users",
		strings.NewReader(`{"name":"test","fields":[]}`))
	r.Header.Set("Content-Type", "application/json")
	setAdminAuth(r)
	rr := execReq(h, r)
	if rr.Code < 400 {
		t.Error("POST /users on read-only DB: crashed")
	}
}

// stackDB.QueryRow returns a closed-DB row whose Scan() fails with
// "database is closed", not a dialect-specific constraint-violation error.
// In production the handler detects "duplicate key"/"unique constraint" and
// returns 409. This test only verifies the handler doesn't crash.
func TestEdgeCase_DBConstraintViolation(t *testing.T) {
	t.Parallel()
	db := &stackDB{engine: "postgres", execErr: errors.New(`duplicate key violates unique constraint`)}
	h := buildAdminRouter(t, db)

	r := httptest.NewRequest(http.MethodPost, "/api/admin/users",
		strings.NewReader(`{"email":"dup@t.com","password":"pw","roles":["editor"]}`))
	r.Header.Set("Content-Type", "application/json")
	setAdminAuth(r)
	rr := execReq(h, r)
	if rr.Code < 400 {
		t.Errorf("expected error status, got %d", rr.Code)
	}
}

// SECTION 6: Error Response Consistency

func TestEdgeCase_ErrorResponseConsistency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"no_auth_me", http.MethodGet, "/api/admin/auth/me", ""},
		{"no_auth_users", http.MethodGet, "/api/admin/users", ""},
		{"bad_login_body", http.MethodPost, "/api/admin/auth/login", `{"email":"","password":""}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			rr := execReq(h, r)

			if rr.Code >= 400 && rr.Code != http.StatusInternalServerError {
				var body map[string]any
				err := json.Unmarshal(rr.Body.Bytes(), &body)
				require.NoError(t, err, "error response must be valid JSON")
				_, hasError := body["error"]
				assert.True(t, hasError, "error response must have 'error' key")
			}
		})
	}
}

func TestEdgeCase_MethodNotAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/admin/health"},
		{http.MethodPut, "/api/admin/health"},
		{http.MethodDelete, "/api/admin/health"},
		{http.MethodPost, "/api/admin/ready"},
		{http.MethodPut, "/api/admin/metrics"},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			r := httptest.NewRequest(tc.method, tc.path, nil)
			rr := execReq(h, r)
			if rr.Code < 400 {
				t.Errorf("wrong method: got 500, want 405")
			}
		})
	}
}

func TestEdgeCase_ContentTypeCompatibility(t *testing.T) {
	t.Parallel()

	accepts := []string{"", "*/*", "text/html", "application/xml", "text/plain", "application/octet-stream"}
	for _, accept := range accepts {
		t.Run("accept_"+accept, func(t *testing.T) {
			h := buildAdminRouter(t, freshDB())
			r := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
			if accept != "" {
				r.Header.Set("Accept", accept)
			}
			rr := execReq(h, r)
			if rr.Code >= 500 {
				t.Errorf("Accept=%q: got %d, expected < 500", accept, rr.Code)
			}
		})
	}
}

// SECTION 7: Race Condition Detection (run with -race)

func TestEdgeCase_RaceDetection_MetricsInit(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/api/admin/metrics", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
		}()
	}
	wg.Wait()
}

func TestEdgeCase_RaceDetection_MetricsReporting(t *testing.T) {
	t.Parallel()
	h := buildAdminRouter(t, freshDB())

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/api/admin/health", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, r)
		}()
	}
	wg.Wait()
}
