//go:build !short && !mutest

package middleware_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// TestAPIKeyAuth_WithScope_EnforcesPermissions verifies the full middleware
// pipeline: APIKeyAuth (with lookup callback) -> RequireScoped. Tests that
// in-scope endpoints are granted access and out-of-scope endpoints are
// denied with 403.
//
// The lookup callback reads PostgreSQL TEXT[] columns as strings and
// converts them to []string via parsePGArray. This works around a pgx
// stdlib limitation where TEXT[] is scanned as a string (e.g. "{read,write}")
// rather than []string. A production key store reads TEXT[] per dialect.
func TestAPIKeyAuth_WithScope_EnforcesPermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	host := testhost.New(pool)
	ctx := context.Background()

	// A minimal API-key table for the lookup callback.
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS sys_api_keys (
			id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name          TEXT NOT NULL,
			key_hash      TEXT NOT NULL UNIQUE,
			roles         TEXT[] NOT NULL DEFAULT '{}',
			schemas       TEXT[] NOT NULL DEFAULT '{}',
			scopes        TEXT[] NOT NULL DEFAULT '{}',
			enabled       BOOLEAN NOT NULL DEFAULT TRUE,
			monthly_limit BIGINT NOT NULL DEFAULT 0,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			expires_at    TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS sys_api_keys_key_hash_idx ON sys_api_keys (key_hash)`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("create sys_api_keys: %v", err)
		}
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS sys_api_keys CASCADE")
	})

	// Helper: insert an API key row with TEXT[] columns as PG array literals
	// and return the raw key + key ID.
	insertKey := func(name, rolesLiteral, scopesLiteral string) (rawKey, keyID string) {
		raw, hash, err := security.GenerateKey()
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}
		row, qrErr := host.Querier(ctx).QueryRow(ctx,
			`INSERT INTO sys_api_keys (name, key_hash, roles, schemas, scopes)
			 VALUES ($1, $2, $3, '{}', $4) RETURNING id`,
			name, hash, rolesLiteral, scopesLiteral,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&keyID)
		if err != nil {
			t.Fatalf("insert key %q: %v", name, err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), "DELETE FROM sys_api_keys WHERE id = $1", keyID)
		})
		return raw, keyID
	}

	// Build the lookup callback. Reads TEXT[] columns as PG string literals
	// (working around pgx stdlib limitation) and converts them to []string.
	lookupFn := middleware.APIKeyLookupFn(func(ctx context.Context, hash string) (*core.AuthClaims, error) {
		var id, name, roleStr, scopeStr string
		var enabled bool
		row, qrErr := host.Querier(ctx).QueryRow(ctx,
			`SELECT id, name,
			        COALESCE(roles::TEXT,  '{}'),
			        COALESCE(scopes::TEXT, '{}'),
			        enabled
			   FROM sys_api_keys WHERE key_hash = $1`, hash,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&id, &name, &roleStr, &scopeStr, &enabled)
		if err != nil || !enabled {
			return nil, nil
		}
		return &core.AuthClaims{
			UserID:   id,
			Email:    "apikey:" + name,
			Roles:    parsePGArray(roleStr),
			Scopes:   parsePGArray(scopeStr),
			IsAPIKey: true,
		}, nil
	})

	// Middleware stack: APIKeyAuth -> RequireScoped -> handler.
	apiKeyMW := middleware.APIKeyAuth(lookupFn)
	handler := apiKeyMW(middleware.RequireScoped(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// Key with content:read and content:list.
	scopedRaw, _ := insertKey(
		fmt.Sprintf("scope_test_%d", time.Now().UnixNano()),
		"{editor}",
		"{content:read,content:list}",
	)

	t.Run("allowed scope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		req.Header.Set("X-API-Key", scopedRaw)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for GET /content/posts, got %d: %s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("denied scope", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/content/posts/123", nil)
		req.Header.Set("X-API-Key", scopedRaw)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("expected 403 for DELETE with content:read only, got %d", rec.Code)
		}
	})

	t.Run("no api key", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		// No auth at all: RequireScoped passes through for nil claims
		// (JWT middleware would handle 401 for protected routes).
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for unauthenticated request, got %d", rec.Code)
		}
	})

	t.Run("wildcard scope grants all", func(t *testing.T) {
		wildcardRaw, _ := insertKey(
			fmt.Sprintf("wildcard_test_%d", time.Now().UnixNano()),
			"{admin}",
			"{*:*}",
		)
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/content/posts/123", nil)
		req.Header.Set("X-API-Key", wildcardRaw)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200 for DELETE with *:* scope, got %d", rec.Code)
		}
	})
}

// parsePGArray converts a PostgreSQL array literal like "{foo,bar}" to
// []string. Handles empty arrays "{}" -> nil. Does not handle escaped
// commas or quotes: sufficient for API key scope/role arrays.
func parsePGArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" {
		return nil
	}
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
