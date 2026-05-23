//go:build integration
// +build integration

// Auth flow integration tests covering JWT authentication.
//
// These tests verify the JWT flow end to end against real databases: setup,
// login, a protected endpoint, and rejection of an expired or invalid token.
//
// Requires: testcontainers (Docker). Run with:
//
//	go test -tags=integration -run TestAuthFlow ./tests/ -v

package tests

import (
	"context"
	"net/http"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// JWT auth flow

func TestAuthFlow_JWT_Postgres(t *testing.T) {
	pool := testdb.Postgres(t)
	testJWTAuthFlow(t, pool)
}

func TestAuthFlow_JWT_MySQL(t *testing.T) {
	pool := testdb.MySQL(t)
	testJWTAuthFlow(t, pool)
}

func TestAuthFlow_JWT_MSSQL(t *testing.T) {
	pool := testdb.MSSQL(t)
	testJWTAuthFlow(t, pool)
}

func testJWTAuthFlow(t *testing.T, pool db.DB) {
	t.Helper()
	ctx := context.Background()
	cfg := testConfig()

	pool.Exec(ctx, "DELETE FROM sys_users") //nolint:errcheck

	adminRouter, err := api.NewAdminRouter(pool, cfg)
	if err != nil {
		t.Fatalf("admin router: %v", err)
	}

	// Setup

	t.Run("setup_creates_admin_and_returns_token", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "POST", "/api/admin/setup",
			mustJSON(t, map[string]string{"email": "jwt-admin@test.local", "password": "securePass123!"}), setupHeaders)
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup: %d %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if _, ok := body["token"]; !ok {
			t.Error("expected token in setup response")
		}
	})

	// Login

	t.Run("login_success", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "POST", "/api/admin/auth/login",
			mustJSON(t, map[string]string{"email": "jwt-admin@test.local", "password": "securePass123!"}), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("login: %d %s", rr.Code, rr.Body)
		}
	})

	t.Run("login_wrong_password_returns_401", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "POST", "/api/admin/auth/login",
			mustJSON(t, map[string]string{"email": "jwt-admin@test.local", "password": "wrong"}), nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rr.Code)
		}
	})

	t.Run("login_nonexistent_user_returns_401", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "POST", "/api/admin/auth/login",
			mustJSON(t, map[string]string{"email": "nobody@test.local", "password": "x"}), nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rr.Code)
		}
	})

	// Protected endpoints

	var adminToken string

	t.Run("login_returns_valid_token", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "POST", "/api/admin/auth/login",
			mustJSON(t, map[string]string{"email": "jwt-admin@test.local", "password": "securePass123!"}), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("login: %d %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		adminToken, _ = body["token"].(string)
		if adminToken == "" {
			t.Fatal("no token in login response")
		}
	})

	t.Run("protected_endpoint_with_token", func(t *testing.T) {
		if adminToken == "" {
			t.Skip("no token")
		}
		rr := doJSON(t, adminRouter, "GET", "/api/admin/auth/me", "",
			map[string]string{"Authorization": "Bearer " + adminToken})
		if rr.Code != http.StatusOK {
			t.Fatalf("me: %d %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if body["email"] != "jwt-admin@test.local" {
			t.Errorf("expected email jwt-admin@test.local, got %v", body["email"])
		}
	})

	t.Run("protected_endpoint_without_token_returns_401", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "GET", "/api/admin/auth/me", "", nil)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rr.Code)
		}
	})

	t.Run("protected_endpoint_with_garbage_token_returns_401", func(t *testing.T) {
		rr := doJSON(t, adminRouter, "GET", "/api/admin/auth/me", "",
			map[string]string{"Authorization": "Bearer not.a.valid.jwt"})
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", rr.Code)
		}
	})

	t.Run("api_token_endpoint_returns_token", func(t *testing.T) {
		apiRouter, err := api.NewAPIRouter(pool, testConfig(), hooks.NewRegistry())
		if err != nil {
			t.Fatalf("api router: %v", err)
		}
		rr := doJSON(t, apiRouter, "POST", "/api/v1/auth/token",
			mustJSON(t, map[string]string{"email": "jwt-admin@test.local", "password": "securePass123!"}), nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("token: %d %s", rr.Code, rr.Body)
		}
		body := parseBody(t, rr)
		if _, ok := body["token"]; !ok {
			t.Error("expected token in response")
		}
		if _, ok := body["expires_in"]; !ok {
			t.Error("expected expires_in in response")
		}
	})
}
