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

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// Benchmark infrastructure: standalone PG container for benchmarks

// tbDB spins up a postgres container, migrates, and returns a connected db.DB.
func tbDB(tb testing.TB) db.DB {
	tb.Helper()

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

func BenchmarkEngine_Health(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/health", "")
		if r.Code >= 400 {
			b.Fatalf("Health returned %d", r.Code)
		}
	}
}

func BenchmarkEngine_AuthMe(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/auth/me", "")
		if r.Code != http.StatusOK {
			b.Fatalf("AuthMe returned %d: %s", r.Code, r.Body.String())
		}
	}
}

func BenchmarkEngine_Schemas(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/schemas", "")
		if r.Code >= 400 {
			b.Fatalf("Schemas returned %d", r.Code)
		}
	}
}

func BenchmarkEngine_PluginsStatus(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/plugins/status", "")
		if r.Code >= 400 {
			b.Fatalf("PluginsStatus returned %d", r.Code)
		}
	}
}

func BenchmarkEngine_Entitlements(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/entitlements", "")
		if r.Code >= 400 {
			b.Fatalf("Entitlements returned %d", r.Code)
		}
	}
}

func BenchmarkEngine_ContentCreate(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		body := fmt.Sprintf(`{"title":"Bench %d","slug":"bench-%d","status":"published"}`, b.N, b.N)
		r := ctx.doJSON(http.MethodPost, "/api/admin/content/"+cmsBenchSchema, body)
		if r.Code >= 400 {
			b.Fatalf("Create returned %d: %s", r.Code, r.Body.String())
		}
	}
}

func BenchmarkEngine_ContentGet(b *testing.B) {
	ctx := newCMSBenchContext(b)
	// Create one doc to read repeatedly.
	rec := ctx.doJSON(http.MethodPost, "/api/admin/content/"+cmsBenchSchema,
		`{"title":"Read Target","slug":"read-target","status":"published"}`)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/content/"+cmsBenchSchema+"/"+id, "")
		if r.Code >= 400 {
			b.Fatalf("Get returned %d", r.Code)
		}
	}
}

func BenchmarkEngine_ContentList(b *testing.B) {
	ctx := newCMSBenchContext(b)
	for i := range 10 {
		ctx.doJSON(http.MethodPost, "/api/admin/content/"+cmsBenchSchema,
			fmt.Sprintf(`{"title":"Item %d","slug":"item-%d","status":"published"}`, i, i))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/content/"+cmsBenchSchema+"?limit=10", "")
		if r.Code >= 400 {
			b.Fatalf("List returned %d: %s", r.Code, r.Body.String())
		}
	}
}

func BenchmarkEngine_ContentUpdate(b *testing.B) {
	ctx := newCMSBenchContext(b)
	rec := ctx.doJSON(http.MethodPost, "/api/admin/content/"+cmsBenchSchema,
		`{"title":"Update Target","slug":"update-target","status":"draft"}`)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodPatch, "/api/admin/content/"+cmsBenchSchema+"/"+id,
			fmt.Sprintf(`{"title":"Updated %d","status":"published"}`, b.N))
		if r.Code >= 400 {
			b.Fatalf("Update returned %d: %s", r.Code, r.Body.String())
		}
	}
}

func BenchmarkEngine_ContentDelete(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec := ctx.doJSON(http.MethodPost, "/api/admin/content/"+cmsBenchSchema,
			fmt.Sprintf(`{"title":"Del %d","slug":"del-%d","status":"draft"}`, b.N, b.N))
		var created map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		id, _ := created["id"].(string)

		r := ctx.doJSON(http.MethodDelete, "/api/admin/content/"+cmsBenchSchema+"/"+id, "")
		if r.Code >= 400 {
			b.Fatalf("Delete returned %d: %s", r.Code, r.Body.String())
		}
	}
}

func BenchmarkEngine_UsersList(b *testing.B) {
	ctx := newCMSBenchContext(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r := ctx.doJSON(http.MethodGet, "/api/admin/users", "")
		if r.Code >= 400 {
			b.Fatalf("Users returned %d: %s", r.Code, r.Body.String())
		}
	}
}
