//go:build !short && !mutest

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// PostgreSQL schema tenancy over tables that hold authentication data.
//
// Covers:
//  1. TOTP secrets: tenant A's secret invisible to tenant B
//  2. Identity providers: tenant A's provider list invisible to tenant B
//  3. Passkeys: tenant A's credentials invisible to tenant B
//  4. Several tables at once, in both directions
//
// Each table here is created inside every tenant's schema, so these tests
// prove that TenancyConn points a request's connection at its own tenant's
// copy. A table in the default schema, where sys_ tables live, is isolated by
// its tenant_id column instead, and these tests do not cover that.

// testTenantKey is a local context key for direct DB isolation tests.
type testTenantKey struct{}

// createTenantSchemaWithTables provisions a PG schema and creates the
// fixture tables inside it.
func createTenantSchemaWithTables(t *testing.T, pool db.DB, schema string) {
	t.Helper()
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	// TOTP secrets and passkeys, shaped the way an MFA store keeps them.
	mfaSQL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.test_totp_secrets (
			user_id      UUID PRIMARY KEY,
			totp_secret  TEXT,
			backup_codes TEXT,
			enabled      BOOLEAN NOT NULL DEFAULT FALSE,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS %s.test_passkeys (
			id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			user_id           UUID NOT NULL REFERENCES %s.test_totp_secrets(user_id) ON DELETE CASCADE,
			credential_id     BYTEA NOT NULL UNIQUE,
			public_key        BYTEA NOT NULL,
			attestation_type  TEXT NOT NULL DEFAULT '',
			attestation_format TEXT NOT NULL DEFAULT '',
			transports        TEXT[] DEFAULT '{}',
			flags             JSONB NOT NULL DEFAULT '{}',
			authenticator     JSONB NOT NULL DEFAULT '{}',
			attestation       JSONB NOT NULL DEFAULT '{}',
			name              TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_used_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`, schema, schema, schema)
	if _, err := pool.Exec(ctx, mfaSQL); err != nil {
		t.Fatalf("create MFA tables in %s: %v", schema, err)
	}

	// Identity providers, shaped the way a sign-in provider list is kept.
	oauthSQL := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s.test_identity_providers (
			id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
			name             TEXT        NOT NULL,
			client_id        TEXT        NOT NULL,
			client_secret_enc TEXT       NOT NULL,
			issuer_url       TEXT        NOT NULL,
			scopes           JSONB       NOT NULL DEFAULT '["openid","email","profile"]',
			roles_claim      TEXT        NOT NULL DEFAULT 'roles',
			default_roles    JSONB       NOT NULL DEFAULT '["editor"]',
			enabled          BOOLEAN     NOT NULL DEFAULT TRUE,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`, schema)
	if _, err := pool.Exec(ctx, oauthSQL); err != nil {
		t.Fatalf("create OAuth table in %s: %v", schema, err)
	}
}

// seedMFASecret inserts a TOTP MFA row into the given tenant schema.
func seedMFASecret(t *testing.T, pool db.DB, schema string, userID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		fmt.Sprintf(`INSERT INTO %s.test_totp_secrets (user_id, totp_secret, enabled, created_at, updated_at)
			VALUES ($1, $2, true, $3, $3)`, schema),
		userID, "encrypted-totp-secret-for-"+schema, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("seed MFA secret in %s: %v", schema, err)
	}
}

// seedWebAuthnCredential inserts a WebAuthn credential row into the given tenant schema.
func seedWebAuthnCredential(t *testing.T, pool db.DB, schema string, userID uuid.UUID, credID []byte) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		fmt.Sprintf(`INSERT INTO %s.test_passkeys
			(id, user_id, credential_id, public_key, attestation_type, attestation_format, name, created_at, last_used_at)
			VALUES ($1, $2, $3, $4, 'none', 'packed', 'Test Passkey', $5, $5)`, schema),
		uuid.New(), userID, credID, []byte("fake-public-key"), time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("seed WebAuthn credential in %s: %v", schema, err)
	}
}

// seedOAuthProvider inserts an OAuth provider row into the given tenant schema.
func seedOAuthProvider(t *testing.T, pool db.DB, schema string, name string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		fmt.Sprintf(`INSERT INTO %s.test_identity_providers
			(id, name, client_id, client_secret_enc, issuer_url, enabled, created_at, updated_at)
			VALUES ($1, $2, 'test-client-id', 'encrypted-secret', 'https://issuer.example.com', true, $3, $3)`, schema),
		uuid.New(), name, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("seed OAuth provider in %s: %v", schema, err)
	}
}

// Test 1: MFA cross-tenant secret isolation

func TestSchemaTenancy_IsolatesTOTPSecrets(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	tenantA := fmt.Sprintf("mfa_a_%d", ts%100000)
	tenantB := fmt.Sprintf("mfa_b_%d", ts%100000)
	schemaA := "tenant_" + tenantA
	schemaB := "tenant_" + tenantB

	createTenantSchemaWithTables(t, pool, schemaA)
	createTenantSchemaWithTables(t, pool, schemaB)
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaA+" CASCADE")
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaB+" CASCADE")
	})

	// Tenant A has an MFA secret. Tenant B has none.
	userA := uuid.New()
	seedMFASecret(t, pool, schemaA, userA)

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentTenant := mw.TenantIDFromContext(r.Context())
		var count int
		row, qrErr := pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM test_totp_secrets")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Errorf("tenant %q: query mfa_secrets error: %v", currentTenant, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-MFA-Count", fmt.Sprintf("%d", count))
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	t.Run("tenant_A_sees_own_mfa_secret", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/status", nil)
		claims := &auth.Claims{TenantID: tenantA, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-MFA-Count"); got != "1" {
			t.Errorf("tenant A should see 1 MFA secret, got %s", got)
		}
	})

	t.Run("tenant_B_sees_zero_mfa_secrets", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/status", nil)
		claims := &auth.Claims{TenantID: tenantB, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-MFA-Count"); got != "0" {
			t.Errorf("CRITICAL: tenant B sees tenant A's MFA secret! count=%s", got)
		}
	})

	// Direct DB-level verification: tenant B's connection has a different search_path,
	// so even with a known user_id the query returns no rows.
	t.Run("tenant_B_direct_query_returns_no_rows", func(t *testing.T) {
		conn, err := pool.SQLDB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		directTenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
			if v, ok := ctx.Value(testTenantKey{}).(string); ok {
				return v
			}
			return ""
		})

		ctxB := context.WithValue(ctx, testTenantKey{}, tenantB)
		if err := directTenancy.Apply(ctxB, conn); err != nil {
			t.Fatalf("apply tenancy: %v", err)
		}

		var count int
		err = conn.QueryRowContext(ctxB,
			"SELECT COUNT(*) FROM test_totp_secrets WHERE user_id = $1", userA,
		).Scan(&count)
		if err != nil {
			t.Fatalf("tenant B query: %v", err)
		} else if count > 0 {
			t.Errorf("CRITICAL: tenant B read tenant A's MFA secret via direct query! count=%d", count)
		}
	})
}

// Test 2: OAuth provider cross-tenant isolation

func TestSchemaTenancy_IsolatesIdentityProviders(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	tenantA := fmt.Sprintf("oauth_a_%d", ts%100000)
	tenantB := fmt.Sprintf("oauth_b_%d", ts%100000)
	schemaA := "tenant_" + tenantA
	schemaB := "tenant_" + tenantB

	createTenantSchemaWithTables(t, pool, schemaA)
	createTenantSchemaWithTables(t, pool, schemaB)
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaA+" CASCADE")
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaB+" CASCADE")
	})

	// Tenant A has two OAuth providers. Tenant B has none.
	seedOAuthProvider(t, pool, schemaA, "google")
	seedOAuthProvider(t, pool, schemaA, "github")

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentTenant := mw.TenantIDFromContext(r.Context())
		var count int
		row, qrErr := pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM test_identity_providers")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Errorf("tenant %q: query oauth_providers error: %v", currentTenant, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-OAuth-Count", fmt.Sprintf("%d", count))
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	t.Run("tenant_A_sees_own_oauth_providers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/oauth/providers", nil)
		claims := &auth.Claims{TenantID: tenantA, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-OAuth-Count"); got != "2" {
			t.Errorf("tenant A should see 2 OAuth providers, got %s", got)
		}
	})

	t.Run("tenant_B_sees_empty_oauth_providers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/oauth/providers", nil)
		claims := &auth.Claims{TenantID: tenantB, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-OAuth-Count"); got != "0" {
			t.Errorf("CRITICAL: tenant B sees tenant A's OAuth providers! count=%s", got)
		}
	})

	t.Run("tenant_B_cannot_see_provider_by_name", func(t *testing.T) {
		conn, err := pool.SQLDB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		directTenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
			if v, ok := ctx.Value(testTenantKey{}).(string); ok {
				return v
			}
			return ""
		})

		ctxB := context.WithValue(ctx, testTenantKey{}, tenantB)
		if err := directTenancy.Apply(ctxB, conn); err != nil {
			t.Fatalf("apply tenancy: %v", err)
		}

		var count int
		err = conn.QueryRowContext(ctxB,
			"SELECT COUNT(*) FROM test_identity_providers WHERE name = $1", "google",
		).Scan(&count)
		if err != nil {
			t.Fatalf("tenant B query: %v", err)
		} else if count > 0 {
			t.Errorf("CRITICAL: tenant B found tenant A's 'google' provider! count=%d", count)
		}
	})
}

// Test 3: MFA WebAuthn credential cross-tenant isolation

func TestSchemaTenancy_IsolatesPasskeys(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	tenantA := fmt.Sprintf("wa_a_%d", ts%100000)
	tenantB := fmt.Sprintf("wa_b_%d", ts%100000)
	schemaA := "tenant_" + tenantA
	schemaB := "tenant_" + tenantB

	createTenantSchemaWithTables(t, pool, schemaA)
	createTenantSchemaWithTables(t, pool, schemaB)
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaA+" CASCADE")
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaB+" CASCADE")
	})

	// Tenant A: user with MFA secret + WebAuthn passkey.
	userA := uuid.New()
	seedMFASecret(t, pool, schemaA, userA)
	credIDA := []byte("credential-tenant-a-001")
	seedWebAuthnCredential(t, pool, schemaA, userA, credIDA)

	// Tenant B: user with MFA secret but NO WebAuthn passkey.
	userB := uuid.New()
	seedMFASecret(t, pool, schemaB, userB)

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentTenant := mw.TenantIDFromContext(r.Context())
		var credCount int
		row, qrErr := pool.QueryRow(r.Context(),
			"SELECT COUNT(*) FROM test_passkeys",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&credCount)
		if err != nil {
			t.Errorf("tenant %q: query webauthn_credentials error: %v", currentTenant, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-WebAuthn-Count", fmt.Sprintf("%d", credCount))
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	t.Run("tenant_A_sees_own_passkey", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/webauthn", nil)
		claims := &auth.Claims{TenantID: tenantA, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-WebAuthn-Count"); got != "1" {
			t.Errorf("tenant A should see 1 WebAuthn credential, got %s", got)
		}
	})

	t.Run("tenant_B_sees_zero_passkeys", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/mfa/webauthn", nil)
		claims := &auth.Claims{TenantID: tenantB, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-WebAuthn-Count"); got != "0" {
			t.Errorf("CRITICAL: tenant B sees tenant A's WebAuthn credentials! count=%s", got)
		}
	})

	t.Run("credential_id_lookup_isolated", func(t *testing.T) {
		conn, err := pool.SQLDB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		directTenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
			if v, ok := ctx.Value(testTenantKey{}).(string); ok {
				return v
			}
			return ""
		})

		ctxB := context.WithValue(ctx, testTenantKey{}, tenantB)
		if err := directTenancy.Apply(ctxB, conn); err != nil {
			t.Fatalf("apply tenancy: %v", err)
		}

		var foundCount int
		err = conn.QueryRowContext(ctxB,
			"SELECT COUNT(*) FROM test_passkeys WHERE credential_id = $1",
			credIDA,
		).Scan(&foundCount)
		if err != nil {
			t.Fatalf("tenant B lookup: %v", err)
		} else if foundCount > 0 {
			t.Errorf("CRITICAL: tenant B found tenant A's WebAuthn credential by credential_id!")
		}
	})
}

// Test 4: Combined MFA + OAuth, full cross-tenant boundary

func TestSchemaTenancy_IsolatesSeveralTablesBothWays(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT != postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()

	tenantA := fmt.Sprintf("combo_a_%d", ts%100000)
	tenantB := fmt.Sprintf("combo_b_%d", ts%100000)
	schemaA := "tenant_" + tenantA
	schemaB := "tenant_" + tenantB

	createTenantSchemaWithTables(t, pool, schemaA)
	createTenantSchemaWithTables(t, pool, schemaB)
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaA+" CASCADE")
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schemaB+" CASCADE")
	})

	// Tenant A: 1 MFA secret, 1 OAuth provider
	userA := uuid.New()
	seedMFASecret(t, pool, schemaA, userA)
	seedOAuthProvider(t, pool, schemaA, "google")

	// Tenant B: 1 MFA secret, 1 OAuth provider (different data)
	userB := uuid.New()
	seedMFASecret(t, pool, schemaB, userB)
	seedOAuthProvider(t, pool, schemaB, "gitlab")

	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		return mw.TenantIDFromContext(ctx)
	})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		currentTenant := mw.TenantIDFromContext(r.Context())

		var mfaCount int
		row, qrErr := pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM test_totp_secrets")
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		if err := row.Scan(&mfaCount); err != nil {
			t.Errorf("tenant %q: mfa count: %v", currentTenant, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		var oauthNames []string
		rows, err := pool.Query(r.Context(), "SELECT name FROM test_identity_providers ORDER BY name")
		if err != nil {
			t.Errorf("tenant %q: oauth query: %v", currentTenant, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			rows.Scan(&name)
			oauthNames = append(oauthNames, name)
		}

		w.Header().Set("X-MFA-Count", fmt.Sprintf("%d", mfaCount))
		w.Header().Set("X-OAuth-Names", fmt.Sprintf("%v", oauthNames))
		w.WriteHeader(http.StatusOK)
	})

	m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

	t.Run("tenant_A_sees_own_data_only", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/status", nil)
		claims := &auth.Claims{TenantID: tenantA, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-MFA-Count"); got != "1" {
			t.Errorf("tenant A MFA count: expected 1, got %s", got)
		}
		names := w.Header().Get("X-OAuth-Names")
		if names != "[google]" {
			t.Errorf("tenant A OAuth names: expected [google], got %s", names)
		}
	})

	t.Run("tenant_B_sees_own_data_only", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/status", nil)
		claims := &auth.Claims{TenantID: tenantB, Roles: []string{"editor"}}
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		if got := w.Header().Get("X-MFA-Count"); got != "1" {
			t.Errorf("tenant B MFA count: expected 1, got %s", got)
		}
		names := w.Header().Get("X-OAuth-Names")
		if names != "[gitlab]" {
			t.Errorf("tenant B OAuth names: expected [gitlab], got %s", names)
		}
	})

	// Cross-verification: even if tenant A queries with B's data identifiers,
	// it can only see its own schema.
	t.Run("no_cross_tenant_data_leakage_via_known_ids", func(t *testing.T) {
		conn, err := pool.SQLDB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()

		directTenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
			if v, ok := ctx.Value(testTenantKey{}).(string); ok {
				return v
			}
			return ""
		})

		ctxA := context.WithValue(ctx, testTenantKey{}, tenantA)
		if err := directTenancy.Apply(ctxA, conn); err != nil {
			t.Fatalf("apply tenancy: %v", err)
		}

		// Tenant A should NOT see "gitlab" (it's in tenant B's schema).
		var count int
		err = conn.QueryRowContext(ctxA,
			"SELECT COUNT(*) FROM test_identity_providers WHERE name = $1", "gitlab",
		).Scan(&count)
		if err != nil {
			t.Fatalf("tenant A query: %v", err)
		} else if count > 0 {
			t.Errorf("CRITICAL: tenant A found tenant B's 'gitlab' provider! count=%d", count)
		}

		// Tenant A should see "google" (its own).
		err = conn.QueryRowContext(ctxA,
			"SELECT COUNT(*) FROM test_identity_providers WHERE name = $1", "google",
		).Scan(&count)
		if err != nil {
			t.Errorf("tenant A should find own 'google' provider: %v", err)
		} else if count != 1 {
			t.Errorf("tenant A should have 1 'google' provider, got %d", count)
		}
	})
}
