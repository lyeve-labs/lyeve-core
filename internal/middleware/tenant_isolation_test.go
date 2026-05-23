//go:build !short && !mutest

//lint:file-ignore SA1029 Using string context keys for cross-package compatibility.

// Tenant isolation through TenantHeader and TenancyConn: API-key claims scope
// the connection as JWT claims do, a super_admin's override is checked against
// the roster, and a slug that could reach SQL is refused.
package middleware_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// TestTenantHeader_APIKeyClaimsScopeTheConnection checks that API-key claims
// (*core.AuthClaims) resolve the tenant and scope the connection exactly as
// JWT claims (*auth.Claims) do. The two live under different context keys, so
// each credential is driven through the full chain against a real database.
func TestTenantHeader_APIKeyClaimsScopeTheConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()
	ts := time.Now().UnixNano()
	slug := fmt.Sprintf("ti_%d", ts%10000)
	schema := "tenant_" + slug

	// Provision tenant schema with identifiable data
	_, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_, err = pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+schema+".ti_data (id SERIAL PRIMARY KEY, label TEXT)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	secretLabel := fmt.Sprintf("SECRET_TI_%d", ts%100000)
	_, err = pool.Exec(ctx, "INSERT INTO "+schema+".ti_data (label) VALUES ($1)", secretLabel)
	if err != nil {
		t.Fatalf("seed data: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	tenancy := db.NewPostgresSchemaTenancy(mw.TenantIDFromContext)

	// countRows serves one request through the chain and counts ti_data rows.
	// The query names the table unqualified, so it finds the row only when the
	// connection was scoped to the tenant's schema.
	countRows := func(t *testing.T, key, claims any) (tenantID string, count int, queryErr error) {
		t.Helper()
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID = mw.TenantIDFromContext(r.Context())
			row, err := pool.QueryRow(r.Context(), "SELECT COUNT(*) FROM ti_data")
			if err != nil {
				queryErr = err
			} else {
				queryErr = row.Scan(&count)
			}
			w.WriteHeader(http.StatusOK)
		})

		m := mw.TenantHeader(true)(mw.TenancyConn(pool, tenancy)(handler))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req = req.WithContext(context.WithValue(req.Context(), key, claims))
		m.ServeHTTP(httptest.NewRecorder(), req)
		return tenantID, count, queryErr
	}

	t.Run("APIKey_claims_scope_the_connection", func(t *testing.T) {
		apiKeyClaims := &core.AuthClaims{
			UserID:   "apikey-00000000-0000-0000-0000-000000000001",
			TenantID: slug,
			Roles:    []string{"editor"},
		}

		tid, count, queryErr := countRows(t, core.ClaimsKey, apiKeyClaims)

		if tid != slug {
			t.Fatalf("API key: tenantID = %q, want %q", tid, slug)
		}
		if queryErr != nil {
			t.Fatalf("API key: unexpected query error: %v", queryErr)
		}
		if count != 1 {
			t.Errorf("API key: expected 1 row from the tenant's schema, got %d", count)
		}
	})

	t.Run("JWT_claims_scope_the_connection", func(t *testing.T) {
		jwtClaims := &auth.Claims{
			TenantID: slug,
			Roles:    []string{"admin"},
		}

		tid, count, queryErr := countRows(t, auth.ClaimsKey, jwtClaims)

		if tid != slug {
			t.Fatalf("JWT: tenantID = %q, want %q", tid, slug)
		}
		if queryErr != nil {
			t.Fatalf("JWT: unexpected query error: %v", queryErr)
		}
		if count != 1 {
			t.Errorf("JWT: expected 1 row from the tenant's schema, got %d", count)
		}
	})
}

// TestTenantHeader_SuperAdminOverrideIsCheckedAgainstTheRoster covers a
// super_admin's X-Tenant-ID override, which is checked against the tenant
// roster through TenantValidatorFunc: an unknown slug answers 404 and a known
// one is accepted.
func TestTenantHeader_SuperAdminOverrideIsCheckedAgainstTheRoster(t *testing.T) {
	realSlug := "real_tenant"
	fakeSlug := "nonexistent_tenant"

	claims := &auth.Claims{
		TenantID: realSlug,
		Roles:    []string{"super_admin"},
	}

	// A validator that accepts "real_tenant" but rejects everything else.
	validator := func(ctx context.Context, slug string) bool {
		return slug == realSlug
	}

	t.Run("nonexistent tenant override returns 404", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("handler should not be reached for rejected tenant")
		})

		m := mw.TenantHeader(true, validator)(handler)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req.Header.Set("X-Tenant-ID", fakeSlug)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404 for nonexistent tenant override %q, got %d", fakeSlug, w.Code)
		}
	})

	t.Run("valid tenant override is accepted", func(t *testing.T) {
		var resolvedTenant string
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resolvedTenant = mw.TenantIDFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})

		m := mw.TenantHeader(true, validator)(handler)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req.Header.Set("X-Tenant-ID", realSlug)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 for valid tenant override, got %d", w.Code)
		}
		if resolvedTenant != realSlug {
			t.Errorf("expected X-Tenant-ID override (%q), got %q", realSlug, resolvedTenant)
		}
	})

	t.Run("no validator accepts a well-formed slug", func(t *testing.T) {
		var resolvedTenant string
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resolvedTenant = mw.TenantIDFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})

		// With no roster validator only the shape check runs, so a
		// well-formed slug is accepted.
		m := mw.TenantHeader(true)(handler)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/data", nil)
		req.Header.Set("X-Tenant-ID", fakeSlug)
		req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200 without a validator, got %d", w.Code)
		}
		if resolvedTenant != fakeSlug {
			t.Errorf("expected X-Tenant-ID override (%q), got %q", fakeSlug, resolvedTenant)
		}
	})
}

// testSlugKey carries the slug under test to the tenancy resolver. A defined
// type keeps it from colliding with any other package's context value.
type testSlugKey struct{}

// TestSQLi_SlugBoundary verifies the safeSlugRe regex blocks all
// SQL injection payloads disguised as tenant slugs.
func TestSQLi_SlugBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT is not postgres")
	}

	pool := testdb.Postgres(t)
	tenancy := db.NewPostgresSchemaTenancy(func(ctx context.Context) string {
		s, _ := ctx.Value(testSlugKey{}).(string)
		return s
	})

	payloads := []struct {
		slug    string
		blocked bool // true = rejection expected
		desc    string
	}{
		{`' OR '1'='1`, true, "quote-tick OR 1=1"},
		{`"; DROP TABLE users;--`, true, "double-quote DROP TABLE"},
		{`1 UNION SELECT password FROM users`, true, "UNION SELECT"},
		{`1;--`, true, "semicolon comment"},
		{`admin'--`, true, "auth bypass"},
		{`]`, true, "MSSQL close bracket"},
		{"`", true, "MySQL backtick"},
		// The empty slug is left out: it means no tenant, and Apply treats it
		// as a no-op before the pattern check.
		{`abc\ndef`, true, "newline"},
		{"abc\tdef", true, "tab"},
		{"abc def", true, "space"},
		// Valid slugs
		{"acme", false, "simple valid"},
		{"acme_corp", false, "underscore valid"},
		{"a123", false, "alphanumeric valid"},
	}

	for _, tc := range payloads {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), testSlugKey{}, tc.slug)
			conn, err := pool.Conn(ctx)
			if err != nil {
				t.Skipf("cannot acquire conn: %v", err)
			}
			defer conn.Close()

			err = tenancy.Apply(ctx, conn)
			if tc.blocked && err == nil {
				t.Errorf("SECURITY: slug %q should have been rejected", tc.slug)
			}
			if !tc.blocked && err != nil {
				t.Errorf("FALSE POSITIVE: valid slug %q was rejected: %v", tc.slug, err)
			}
		})
	}
}
