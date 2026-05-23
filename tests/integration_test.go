// Package tests contains integration tests for the CMS.
//
// These tests require a live PostgreSQL database. Set TEST_DATABASE_URL to
// a valid DSN before running:
//
//	TEST_DATABASE_URL="postgres://user:pass@localhost:5432/lyeve_test" go test ./tests/...
//
// If TEST_DATABASE_URL is not set the tests are skipped automatically.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Test helpers

func testPool(t *testing.T) db.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set - skipping integration test")
	}
	pool, err := db.Connect(context.Background(), dsn, 8)
	if err != nil {
		t.Fatalf("connect to test DB: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping test DB: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func testConfig() *config.Config {
	return testConfigFor("postgres")
}

// testConfigFor builds the config for a given driver. The routers read the
// dialect from DatabaseDriver, not from the pool, and an empty value resolves
// to Postgres, so a MySQL or MSSQL pool driven by the default config gets
// PostgreSQL syntax and fails on the first dialect-specific statement.
func testConfigFor(driver string) *config.Config {
	const secret = "test-secret-32-bytes-long-enough!"
	return &config.Config{
		JWTSecret: secret,
		// The routers verify against JWTSecrets. JWTSecret alone only signs.
		// config.Load derives one from the other, which a literal skips, and
		// an empty verification set rejects every token the handlers issue.
		JWTSecrets:       []string{secret},
		DatabaseDriver:   driver,
		JWTExpirySecs:    3600,
		CORSOrigins:      []string{"*"},
		RateLimitRPS:     0,
		PasswordHashAlgo: "bcrypt",
		MaxBodyBytes:     1 << 20,
		SetupToken:       testSetupToken,
	}
}

// testSetupToken is the operator's LYEVE_SETUP_TOKEN in every config these
// tests build. First-run setup refuses a caller who does not present it.
const testSetupToken = "integration-setup-token-0123"

var setupHeaders = map[string]string{"X-Setup-Token": testSetupToken}

func doJSON(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(b)
}

func parseBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Logf("response body: %s", rr.Body.String())
		t.Fatalf("decode response JSON: %v", err)
	}
	return m
}

// supplySchema defines a content type and returns the source the content API
// reads it from. The kernel mounts no route that creates a schema, because
// authoring belongs to the plugin that registers the engine. The suite defines
// its types through an engine built on pkg/core alone, and the content API
// reads them through the same core.SchemaSource a plugin's engine supplies.
func supplySchema(t *testing.T, pool db.DB, name string, fields ...map[string]any) core.SchemaSource {
	t.Helper()
	def := mustJSON(t, map[string]any{"name": name, "fields": fields})
	eng := schemaengine.New(testhost.New(pool))
	if err := eng.Apply(context.Background(), name, json.RawMessage(def)); err != nil {
		t.Fatalf("define schema %s: %v", name, err)
	}
	return eng.SchemaSource()
}

func cleanupSchema(t *testing.T, pool db.DB, name string) {
	t.Helper()
	ctx := context.Background()
	table := domain.TableName(name)
	pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table)) //nolint:errcheck
	// The supplied engine keeps the definition in memory, so the generated
	// table is all there is to clean up.
}

// Auth tests

func TestAuth_SetupAndLogin(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()

	pool.Exec(context.Background(), "DELETE FROM sys_users") //nolint:errcheck

	router, _ := api.NewAdminRouter(pool, cfg)

	t.Run("setup_status_no_users", func(t *testing.T) {
		rr := doJSON(t, router, "GET", "/api/admin/setup", "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if body["setup_required"] != true {
			t.Errorf("expected setup_required=true, got %v", body["setup_required"])
		}
		if body["token_source"] != "env" {
			t.Errorf("expected token_source=env, got %v", body["token_source"])
		}
	})

	t.Run("setup_refused_without_token", func(t *testing.T) {
		for name, headers := range map[string]map[string]string{
			"absent": nil,
			"wrong":  {"X-Setup-Token": "not-the-operator-token"},
		} {
			rr := doJSON(t, router, "POST", "/api/admin/setup",
				mustJSON(t, map[string]string{"email": "intruder@test.local", "password": "password123"}), headers)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s token: expected 401 got %d: %s", name, rr.Code, rr.Body)
			}
		}
		var n int
		row, err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sys_users`)
		if err != nil {
			t.Fatalf("count users: %v", err)
		}
		if err := row.Scan(&n); err != nil {
			t.Fatalf("count users: %v", err)
		}
		if n != 0 {
			t.Fatalf("a refused claim created %d accounts", n)
		}
	})

	var token string

	t.Run("setup_creates_admin", func(t *testing.T) {
		rr := doJSON(t, router, "POST", "/api/admin/setup",
			mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), setupHeaders)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if _, ok := body["token"]; !ok {
			t.Error("expected token in response")
		}
		token = body["token"].(string)
	})

	t.Run("setup_token_retired_after_first_admin", func(t *testing.T) {
		rr := doJSON(t, router, "POST", "/api/admin/setup",
			mustJSON(t, map[string]string{"email": "second@test.local", "password": "password123"}), setupHeaders)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 for the retired token, got %d: %s", rr.Code, rr.Body)
		}
	})

	t.Run("login_success", func(t *testing.T) {
		rr := doJSON(t, router, "POST", "/api/admin/auth/login",
			mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
	})

	t.Run("login_wrong_password", func(t *testing.T) {
		rr := doJSON(t, router, "POST", "/api/admin/auth/login",
			mustJSON(t, map[string]string{"email": "admin@test.local", "password": "wrong"}), nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 got %d", rr.Code)
		}
	})

	t.Run("me_with_token", func(t *testing.T) {
		if token == "" {
			t.Skip("no token from setup")
		}
		rr := doJSON(t, router, "GET", "/api/admin/auth/me", "",
			map[string]string{"Authorization": "Bearer " + token})
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if body["email"] != "admin@test.local" {
			t.Errorf("expected email admin@test.local, got %v", body["email"])
		}
	})
}

// Content CRUD tests

func TestContent_CRUD(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()

	pool.Exec(context.Background(), "DELETE FROM sys_users") //nolint:errcheck
	router, _ := api.NewAdminRouter(pool, cfg)

	rr := doJSON(t, router, "POST", "/api/admin/setup",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), setupHeaders)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", rr.Code, rr.Body)
	}

	const testSchema = "integration_posts"
	t.Cleanup(func() { cleanupSchema(t, pool, testSchema) })
	schemas := supplySchema(t, pool, testSchema,
		map[string]any{"name": "title", "field_type": "text", "required": true})

	// Content operations via API router.
	hookReg := hooks.NewRegistry()

	apiCfg := testConfig()
	apiRouter, _ := api.NewAPIRouter(pool, apiCfg, hookReg, api.WithSchemaSource(schemas))

	// Bearer token from API /auth/token.
	rr = doJSON(t, apiRouter, "POST", "/api/v1/auth/token",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("token failed: %d %s", rr.Code, rr.Body)
	}
	apiToken := parseBody(t, rr)["token"].(string)
	apiHeader := map[string]string{"Authorization": "Bearer " + apiToken}

	var createdID string

	t.Run("create_content", func(t *testing.T) {
		payload := map[string]any{"data": map[string]any{"title": "Hello world"}}
		rr := doJSON(t, apiRouter, "POST", "/api/v1/content/"+testSchema, mustJSON(t, payload), apiHeader)
		if rr.Code != http.StatusCreated {
			t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		createdID = fmt.Sprintf("%v", body["id"])
	})

	t.Run("list_content", func(t *testing.T) {
		rr := doJSON(t, apiRouter, "GET", "/api/v1/content/"+testSchema, "", apiHeader)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
	})

	t.Run("get_content", func(t *testing.T) {
		if createdID == "" {
			t.Skip("no ID from create")
		}
		rr := doJSON(t, apiRouter, "GET", "/api/v1/content/"+testSchema+"/"+createdID, "", apiHeader)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		data := body["data"].(map[string]any)
		if data["title"] != "Hello world" {
			t.Errorf("expected title 'Hello world', got %v", data["title"])
		}
	})

	t.Run("update_content", func(t *testing.T) {
		if createdID == "" {
			t.Skip("no ID from create")
		}
		payload := map[string]any{"data": map[string]any{"title": "Updated title"}}
		rr := doJSON(t, apiRouter, "PUT", "/api/v1/content/"+testSchema+"/"+createdID, mustJSON(t, payload), apiHeader)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
	})

	t.Run("delete_content", func(t *testing.T) {
		if createdID == "" {
			t.Skip("no ID from create")
		}
		rr := doJSON(t, apiRouter, "DELETE", "/api/v1/content/"+testSchema+"/"+createdID, "", apiHeader)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("expected 204 got %d: %s", rr.Code, rr.Body)
		}
	})

	t.Run("cursor_pagination", func(t *testing.T) {
		rr := doJSON(t, apiRouter, "GET", "/api/v1/content/"+testSchema+"/cursor?limit=5", "", apiHeader)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if _, ok := body["data"]; !ok {
			t.Error("expected 'data' key in cursor response")
		}
		if _, ok := body["next_cursor"]; !ok {
			t.Error("expected 'next_cursor' key in cursor response")
		}
	})
}

// Hook abort test

func TestHook_AbortBeforeCreate(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()

	pool.Exec(context.Background(), "DELETE FROM sys_users") //nolint:errcheck
	adminRouter, _ := api.NewAdminRouter(pool, cfg)

	rr := doJSON(t, adminRouter, "POST", "/api/admin/setup",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), setupHeaders)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body)
	}

	const testSchema = "integration_hook_items"
	t.Cleanup(func() { cleanupSchema(t, pool, testSchema) })
	schemas := supplySchema(t, pool, testSchema,
		map[string]any{"name": "value", "field_type": "number"})

	hookReg := hooks.NewRegistry()
	hookReg.Register(testSchema, hooks.BeforeCreate, func(ctx context.Context, e hooks.Event) error {
		return fmt.Errorf("value must be positive")
	})

	apiRouter, _ := api.NewAPIRouter(pool, testConfig(), hookReg, api.WithSchemaSource(schemas))
	rr = doJSON(t, apiRouter, "POST", "/api/v1/auth/token",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), nil)
	apiToken := parseBody(t, rr)["token"].(string)
	apiHeader := map[string]string{"Authorization": "Bearer " + apiToken}

	payload := map[string]any{"data": map[string]any{"value": -1}}
	rr = doJSON(t, apiRouter, "POST", "/api/v1/content/"+testSchema, mustJSON(t, payload), apiHeader)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 (hook abort), got %d: %s", rr.Code, rr.Body)
	}
}

// Probe endpoints test

func TestProbes(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()
	router, _ := api.NewAdminRouter(pool, cfg, api.WithHealthProbes(api.NewProbeRegistry()))

	// The Kubernetes probes mount ahead of JWT auth, but only when a registry
	// is supplied: without the option they are absent, not unauthenticated.
	// /api/admin/health and /api/admin/ready are the authenticated pair.
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			rr := doJSON(t, router, "GET", path, "", nil)
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200 got %d: %s", rr.Code, rr.Body)
			}
		})
	}
}

// Bulk insert test

func TestContent_BulkCreate(t *testing.T) {
	pool := testPool(t)

	pool.Exec(context.Background(), "DELETE FROM sys_users") //nolint:errcheck
	adminRouter, _ := api.NewAdminRouter(pool, testConfig())

	rr := doJSON(t, adminRouter, "POST", "/api/admin/setup",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), setupHeaders)
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body)
	}

	const testSchema = "integration_bulk"
	t.Cleanup(func() { cleanupSchema(t, pool, testSchema) })
	schemas := supplySchema(t, pool, testSchema,
		map[string]any{"name": "label", "field_type": "text"})

	apiRouter, _ := api.NewAPIRouter(pool, testConfig(), hooks.NewRegistry(), api.WithSchemaSource(schemas))
	rr = doJSON(t, apiRouter, "POST", "/api/v1/auth/token",
		mustJSON(t, map[string]string{"email": "admin@test.local", "password": "password123"}), nil)
	apiToken := parseBody(t, rr)["token"].(string)
	apiHeader := map[string]string{"Authorization": "Bearer " + apiToken}

	payload := map[string]any{
		"items": []map[string]any{
			{"label": "Item 1"},
			{"label": "Item 2"},
			{"label": "Item 3"},
		},
	}
	rr = doJSON(t, apiRouter, "POST", "/api/v1/content/"+testSchema+"/bulk", mustJSON(t, payload), apiHeader)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body)
	}

	var results []any
	if err := json.NewDecoder(rr.Body).Decode(&results); err != nil {
		t.Fatalf("decode bulk response: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
}
