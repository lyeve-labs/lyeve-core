//go:build !mutest

// Package api: engine operation benchmarks (content CRUD, plugin ops, schema ops).
//
// These benchmarks measure full-stack operations through the real router
// against a PostgreSQL testcontainer. They complement the micro-benchmarks
// in internal/[auth|db|middleware|pool]/ by covering realistic workflow patterns.
//
// Run with: go test ./internal/api/ -run='^$' -bench=BenchmarkEngine -count=5 -timeout 600s
// Skip in:    go test -short (testcontainers require Docker)
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdialect"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
)

// Benchmark infrastructure: standalone PG container for benchmarks

// tbDB spins up a postgres container, migrates, and returns a connected db.DB.
func tbDB(tb testing.TB) db.DB {
	tb.Helper()
	testdialect.Require(tb, "postgres")

	ctx := context.Background()
	ctr, err := postgres.Run(
		ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("lyeve_bench"),
		postgres.WithUsername("cms"),
		postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		tb.Fatalf("start postgres container: %v", err)
	}
	tb.Cleanup(func() {
		if err := ctr.Terminate(ctx); err != nil {
			tb.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		tb.Fatalf("postgres connection string: %v", err)
	}

	d, err := db.Connect(ctx, dsn, 4)
	if err != nil {
		tb.Fatalf("connect to postgres: %v", err)
	}

	migrationsDir := findMigrationsDir()
	n, err := db.Migrate(dsn, migrationsDir, nil)
	if err != nil {
		d.Close()
		tb.Fatalf("migrate postgres: %v", err)
	}
	tb.Logf("bench postgres: applied %d migrations", n)

	return d
}

func findMigrationsDir() string {
	return "../../migrations"
}

const (
	cmsBenchEmail  = "cms-bench@example.com"
	cmsBenchPass   = "cms-bench-password-12-chars"
	cmsBenchSchema = "bench_posts"
)

type cmsBenchContext struct {
	router http.Handler
	jwt    string
}

func newCMSBenchContext(tb testing.TB) cmsBenchContext {
	tb.Helper()

	pool := tbDB(tb)

	cfg := &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecrets:       []string{"cms-bench-jwt-secret-32bytes!!"},
		JWTSecret:        "cms-bench-jwt-secret-32bytes!!",
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"http://localhost:5173"},
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
		RateLimitRPS:     0,
		SecureCookie:     false,
		PasswordHashAlgo: "bcrypt",
		SetupToken:       cmsBenchSetupToken,
	}

	router, _ := NewAdminRouter(pool, cfg, WithLifetime(testLifetime(tb)))
	jwt := benchSetupUser(tb, router)

	return cmsBenchContext{router: router, jwt: jwt}
}

// benchSetupUser creates a test user and returns a JWT.
const cmsBenchSetupToken = "cms-bench-setup-token-0123"

func benchSetupUser(tb testing.TB, router http.Handler) string {
	tb.Helper()

	setupBody := fmt.Sprintf(`{"email":"%s","password":"%s","name":"Bench User"}`, cmsBenchEmail, cmsBenchPass)
	setupReq := httptest.NewRequest(http.MethodPost, "/api/admin/setup", strings.NewReader(setupBody))
	setupReq.Header.Set("Content-Type", "application/json")
	setupReq.Header.Set(SetupTokenHeader, cmsBenchSetupToken)
	setupReq.RemoteAddr = "192.0.2.1:12345"
	setupRec := httptest.NewRecorder()
	router.ServeHTTP(setupRec, setupReq)
	tb.Logf("benchSetup: /api/admin/setup -> %d", setupRec.Code)

	loginBody := fmt.Sprintf(`{"email":"%s","password":"%s"}`, cmsBenchEmail, cmsBenchPass)
	loginReq := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.RemoteAddr = "192.0.2.1:12345"
	loginRec := httptest.NewRecorder()
	router.ServeHTTP(loginRec, loginReq)

	var resp struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(loginRec.Body).Decode(&resp)

	if resp.Token == "" {
		for _, c := range loginRec.Result().Cookies() {
			if c.Name == "jwt" {
				resp.Token = c.Value
				break
			}
		}
	}

	if resp.Token == "" {
		tb.Logf("benchSetup: login body = %s", loginRec.Body.String())
		tb.Fatal("failed to get JWT for benchmark")
	}

	return resp.Token
}

func (c cmsBenchContext) doJSON(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.jwt != "" {
		req.Header.Set("Authorization", "Bearer "+c.jwt)
	}
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	return rec
}

// Benchmarks

// require2xx stops the benchmark on any answer outside 2xx. Each benchmark
// calls it once before its timed loop, so a route that has moved or a request
// the engine refuses fails the run instead of timing a fast error page.
func require2xx(b *testing.B, rec *httptest.ResponseRecorder, what string) {
	b.Helper()
	if rec.Code < 200 || rec.Code > 299 {
		b.Fatalf("%s returned %d: %s", what, rec.Code, rec.Body.String())
	}
}

// benchGET times one GET, after a first request has proven the route answers.
func benchGET(b *testing.B, do func(method, path, body string) *httptest.ResponseRecorder, path string) {
	b.Helper()
	require2xx(b, do(http.MethodGet, path, ""), "GET "+path)
	b.ReportAllocs()
	for b.Loop() {
		require2xx(b, do(http.MethodGet, path, ""), "GET "+path)
	}
}

func BenchmarkEngine_Health(b *testing.B) {
	benchGET(b, newCMSBenchContext(b).doJSON, "/api/admin/health")
}

func BenchmarkEngine_AuthMe(b *testing.B) {
	benchGET(b, newCMSBenchContext(b).doJSON, "/api/admin/auth/me")
}

func BenchmarkEngine_PluginsStatus(b *testing.B) {
	benchGET(b, newCMSBenchContext(b).doJSON, "/api/admin/plugins/status")
}

func BenchmarkEngine_Entitlements(b *testing.B) {
	benchGET(b, newCMSBenchContext(b).doJSON, "/api/admin/entitlements")
}

func BenchmarkEngine_UsersList(b *testing.B) {
	benchGET(b, newCMSBenchContext(b).doJSON, "/api/admin/users")
}

// contentAPIBench serves the public content API through NewAPIRouter, the
// router the engine mounts, so a request passes the whole kernel chain: auth,
// tenancy, capture, the cache middleware and the handler. Plugins add their
// own middleware on top in a real install, and none is loaded here.
type contentAPIBench struct {
	router http.Handler
	token  string
	ids    []string
}

// contentAPIBenchRows is the page size of the published list scenario.
const contentAPIBenchRows = 50

func newContentAPIBench(b *testing.B) contentAPIBench {
	b.Helper()
	ctx := context.Background()
	pool := tbDB(b)

	eng := schemaengine.New(testhost.New(pool))
	if err := eng.Apply(ctx, cmsBenchSchema, json.RawMessage(`{"name":"`+cmsBenchSchema+`","display_name":"Bench posts","fields":[`+
		`{"name":"title","field_type":"text"},{"name":"slug","field_type":"text"},{"name":"body","field_type":"text"}]}`)); err != nil {
		b.Fatalf("apply schema: %v", err)
	}

	cfg := &config.Config{
		DatabaseDriver:   "postgres",
		JWTSecrets:       []string{testSecret},
		JWTSecret:        testSecret,
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"http://localhost:5173"},
		MaxBodyBytes:     10 << 20,
		MaxJSONBodyBytes: 1 << 20,
		CacheTTL:         60 * time.Second,
		CacheMaxEntries:  1000,
	}
	router, err := NewAPIRouter(pool, cfg, nil,
		WithSchemaSource(eng.SchemaSource()),
		WithContentRules(eng),
		WithLifetime(testLifetime(b)),
	)
	if err != nil {
		b.Fatalf("build api router: %v", err)
	}

	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sys_users (id, email, password_hash, roles, token_version) VALUES ($1, $2, $3, $4, $5)`,
		userID, cmsBenchEmail, "$2a$10$placeholder-hash-for-bench", `{"super_admin"}`, 1); err != nil {
		b.Fatalf("seed user: %v", err)
	}
	c := contentAPIBench{router: router, token: "Bearer " + adminJWT(userID, cmsBenchEmail, []string{"super_admin"})}

	for i := range contentAPIBenchRows {
		c.ids = append(c.ids, c.create(b, i))
	}
	return c
}

func (c contentAPIBench) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", c.token)
	req.RemoteAddr = "192.0.2.1:12345"
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	return rec
}

// BenchmarkEngine_Schemas lists the schema catalog the public API serves.
// The admin router serves no schema listing of its own.
func BenchmarkEngine_Schemas(b *testing.B) {
	c := newContentAPIBench(b)
	benchGET(b, c.do, "/api/v1/schemas")
}

// BenchmarkEngine_ContentGet reads one entry by id through the public content
// API.
func BenchmarkEngine_ContentGet(b *testing.B) {
	c := newContentAPIBench(b)
	benchGET(b, c.do, "/api/v1/content/"+cmsBenchSchema+"/"+c.ids[0])
}

// BenchmarkEngine_ContentList reads a page of 50 entries through the public
// content API.
func BenchmarkEngine_ContentList(b *testing.B) {
	c := newContentAPIBench(b)
	benchGET(b, c.do, fmt.Sprintf("/api/v1/content/%s?limit=%d", cmsBenchSchema, contentAPIBenchRows))
}

func contentAPIBenchBody(n int) string {
	return fmt.Sprintf(`{"data":{"title":"Bench %d","slug":"bench-%d","body":"%s"}}`, n, n, strings.Repeat("lorem ipsum ", 40))
}

// create writes one entry and returns its id, stopping the benchmark on
// anything but a created entry.
func (c contentAPIBench) create(b *testing.B, n int) string {
	b.Helper()
	rec := c.do(http.MethodPost, "/api/v1/content/"+cmsBenchSchema, contentAPIBenchBody(n))
	require2xx(b, rec, "create")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		b.Fatalf("create returned no id: %s", rec.Body.String())
	}
	return created.ID
}

func BenchmarkEngine_ContentCreate(b *testing.B) {
	c := newContentAPIBench(b)
	n := contentAPIBenchRows
	c.create(b, n)

	b.ReportAllocs()
	for b.Loop() {
		n++
		c.create(b, n)
	}
}

func BenchmarkEngine_ContentUpdate(b *testing.B) {
	c := newContentAPIBench(b)
	path := "/api/v1/content/" + cmsBenchSchema + "/" + c.ids[0]
	n := 0
	require2xx(b, c.do(http.MethodPut, path, contentAPIBenchBody(n)), "update")

	b.ReportAllocs()
	for b.Loop() {
		n++
		require2xx(b, c.do(http.MethodPut, path, contentAPIBenchBody(n)), "update")
	}
}

// BenchmarkEngine_ContentDelete times the delete alone. Each iteration writes
// its target with the timer stopped.
func BenchmarkEngine_ContentDelete(b *testing.B) {
	c := newContentAPIBench(b)
	n := contentAPIBenchRows
	require2xx(b, c.do(http.MethodDelete, "/api/v1/content/"+cmsBenchSchema+"/"+c.create(b, n), ""), "delete")

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		n++
		id := c.create(b, n)
		b.StartTimer()
		require2xx(b, c.do(http.MethodDelete, "/api/v1/content/"+cmsBenchSchema+"/"+id, ""), "delete")
	}
}
