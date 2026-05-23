package middleware_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// Tenant isolation through the middleware chain: the slug gate that keeps a
// tenant name out of SQL, how TenantHeader treats each credential, what a
// request carries into the connection pool, and what an answer reveals about
// which tenants exist.

// Tenant slugs
//
// A tenant slug reaches SQL through TenantHeader, TenancyConn and the tenancy
// strategy, which quotes it into an identifier. The strategy refuses any slug
// outside ^[a-z][a-z0-9_]{0,62}$ before it builds SQL, core.IsValidTenantSlug
// holds the same rule for the roster checks, and the quoting doubles an
// embedded quote as a second layer. The slug comes from a verified JWT or from
// an X-Tenant-ID header only a super_admin can set, and it must pass the
// pattern either way.

func TestTenantSlug_InjectionPayloadsAreRefused(t *testing.T) {
	injectionPayloads := []string{
		// Statement termination
		`acme; DROP TABLE users--`,
		`acme'; DROP TABLE users;--`,
		`acme" OR 1=1--`,
		// Quote escape attempts
		`acme"`,
		`acme""`,
		`acme\"`,
		`acme\'`,
		// Schema path traversal (set public search_path to bypass isolation)
		`acme, pg_catalog`,
		`acme, information_schema`,
		// Unicode / encoding attacks
		`acme\u0000`,
		`acme\x00`,
		// Comment injection
		`acme--`,
		`acme/**/`,
		// Newline injection
		"acme\nSET search_path = public",
		// Upper-case bypass attempt
		`ACME`,
		`Acme`,
		// Numeric bypass (starts with digit)
		`123acme`,
		// Maximum length boundary (63 ok, 64 rejected)
		strings.Repeat("a", 64),
		// Spaces (not in regex class)
		`acme corp`,
		// Dash (not in [a-z0-9_] class)
		`acme-corp`,
		// Dot
		`acme.corp`,
	}

	for _, payload := range injectionPayloads {
		t.Run("reject_"+sanitizeName(payload), func(t *testing.T) {
			if core.IsValidTenantSlug(payload) {
				t.Errorf("core.IsValidTenantSlug accepted injection payload %q", payload)
			}
			// A refused slug never reaches the connection, so none is needed.
			tenancy := &db.PostgresSchemaTenancy{TenantIDFunc: func(context.Context) string { return payload }}
			if err := tenancy.Apply(context.Background(), nil); !errors.Is(err, core.ErrValidation) {
				t.Errorf("tenancy Apply(%q) = %v, want a validation error before any SQL", payload, err)
			}
		})
	}
}

func TestTenantSlug_ValidSlugsAreAccepted(t *testing.T) {
	validSlugs := []string{
		"a",
		"acme",
		"acme_corp",
		"tenant123",
		"my_tenant_2",
		"x_y_z",
		strings.Repeat("a", 63),
	}

	for _, slug := range validSlugs {
		t.Run("accept_"+slug, func(t *testing.T) {
			if !core.IsValidTenantSlug(slug) {
				t.Errorf("core.IsValidTenantSlug rejected valid slug %q", slug)
			}
		})
	}
}

func TestQuotePG_EscapesEmbeddedQuotes(t *testing.T) {
	// The second layer: an embedded double quote is doubled, so a slug that
	// got past the pattern still could not end the identifier early.
	cases := []struct{ in, want string }{
		{`acme`, `"acme"`},
		{`with"quote`, `"with""quote"`},
		{`a"b"c`, `"a""b""c"`},
		{`""`, `""""""`},
	}

	for _, c := range cases {
		t.Run("quote_"+c.in, func(t *testing.T) {
			if got := sqlx.QuotePG(c.in); got != c.want {
				t.Errorf("QuotePG(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// "tenant_acme"" OR 1=1" is a single Postgres identifier, tenant_acme" OR
	// 1=1. It holds four double quotes: the opening one, the doubled embedded
	// pair and the closing one.
	quoted := sqlx.QuotePG("tenant_" + `acme" OR 1=1`)
	if n := strings.Count(quoted, `"`); n != 4 {
		t.Errorf("expected 4 double-quotes in escaped identifier, got %d (%s)", n, quoted)
	}
}

// Credentials
//
// TenantHeader takes the tenant from the claims a credential produced, and a
// header moves only a super_admin.

func TestTenantHeader_ClaimTenantIsHonoredAndHeaderCannotMoveIt(t *testing.T) {
	// TenantHeader honors the tenant on the claims it is handed, whichever
	// credential produced them. For a trusted issuer's token those claims come
	// from issuer admission, where the operator's policy names the tenant. A
	// non-super-admin's header cannot move the request anywhere else.
	m := mw.TenantHeader(true)

	var resolved string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	claims := &auth.Claims{TenantID: "claimed-tenant", Roles: []string{"editor"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	m(next).ServeHTTP(httptest.NewRecorder(), req)

	if resolved != "claimed-tenant" {
		t.Fatalf("tenant = %q; want the claim to be honored", resolved)
	}

	// The header cannot be used to move sideways from there: the claim
	// decides, and a non-super-admin's header is overwritten.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	req.Header.Set("X-Tenant-ID", "somewhere_else")
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	m(next).ServeHTTP(httptest.NewRecorder(), req)

	if resolved != "claimed-tenant" {
		t.Fatalf("tenant = %q; a header must not override the claim", resolved)
	}
	if got := req.Header.Get("X-Tenant-ID"); got != "claimed-tenant" {
		t.Errorf("raw header = %q; the middleware must normalize it to the resolved tenant", got)
	}
}

func TestTenantHeader_APIKeyClaimsResolveATenant(t *testing.T) {
	// auth.ClaimsKey and core.ClaimsKey are different Go types with the same
	// underlying string, so a value stored under one is invisible under the
	// other. TenantHeader reads JWT claims from auth.ClaimsKey and API-key
	// claims from core.ClaimsKey, and it has to read both: reading only the
	// first would leave every API-key request with no tenant at all, which is
	// not a smaller blast radius than the wrong tenant. It is a
	// machine-to-machine credential that silently sees nothing.
	m := mw.TenantHeader(true)

	var resolved string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	req = req.WithContext(context.WithValue(req.Context(),
		core.ClaimsKey, &core.AuthClaims{TenantID: "api-key-tenant", Roles: []string{"editor"}}))

	m(next).ServeHTTP(httptest.NewRecorder(), req)

	if resolved != "api-key-tenant" {
		t.Fatalf("tenant = %q; an API key must resolve a tenant like a JWT does", resolved)
	}

	// And the two keys really are distinct, which is why both reads exist.
	ctx := context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{TenantID: "api-key-tenant"})
	if _, ok := ctx.Value(auth.ClaimsKey).(*auth.Claims); ok {
		t.Error("core.ClaimsKey and auth.ClaimsKey collide; one read would have been enough")
	}
}

func TestTenantHeader_NonSuperAdminHeaderIsIgnored(t *testing.T) {
	// Non-super_admin users must not be able to override tenant via header.
	m := mw.TenantHeader(true)
	var resolvedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolvedTenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	req.Header.Set("X-Tenant-ID", "stolen_tenant")
	claims := &auth.Claims{
		TenantID: "legit-tenant",
		Roles:    []string{"admin"}, // NOT super_admin
	}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)
	m(next).ServeHTTP(httptest.NewRecorder(), req)

	if resolvedTenant != "legit-tenant" {
		t.Errorf("non-super_admin override: got %q, want 'legit-tenant'", resolvedTenant)
	}
}

func TestTenantHeader_UnauthenticatedHeaderIsIgnored(t *testing.T) {
	// No JWT claims at all: X-Tenant-ID must be ignored.
	m := mw.TenantHeader(true)
	var resolvedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolvedTenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	req.Header.Set("X-Tenant-ID", "unauthenticated_attack")
	m(next).ServeHTTP(httptest.NewRecorder(), req)

	if resolvedTenant != "" {
		t.Errorf("unauthenticated X-Tenant-ID: got %q, want empty", resolvedTenant)
	}
}

// Connections
//
// TenancyConn scopes a pooled connection to the resolved tenant and resets it
// on the way out. What a failed reset does to a pooled connection needs a real
// database, and tenancy_reset_failure_test.go proves it against one.

func TestTenancyConn_NoResetWithoutAConnection(t *testing.T) {
	// With no database TenancyConn acquires nothing, so a request canceled
	// mid-handler must not trigger a Reset.
	acquired := make(chan struct{}, 1)
	tenancy := recordingTenancy{reset: acquired}

	ctx, cancel := context.WithCancel(context.Background())
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the client goes away before the handler returns
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil).WithContext(ctx)
	req = req.WithContext(context.WithValue(req.Context(),
		auth.ClaimsKey, &auth.Claims{TenantID: "tenant-a"}))

	mw.TenantHeader(true)(mw.TenancyConn(nil, tenancy)(next)).ServeHTTP(httptest.NewRecorder(), req)

	// A nil DB makes TenancyConn a pass-through, so nothing was acquired and
	// nothing needs resetting. The assertion that matters is the negative one:
	// no reset without an acquisition.
	select {
	case <-acquired:
		t.Fatal("Reset ran without a connection having been acquired")
	default:
	}
}

// recordingTenancy reports when Reset runs. It is deliberately not a database:
// what a failed reset does to a pooled connection needs a real one, and lives
// in tenancy_reset_failure_test.go.
type recordingTenancy struct{ reset chan struct{} }

func (recordingTenancy) Apply(context.Context, *sql.Conn) error { return nil }

func (r recordingTenancy) Reset(context.Context, *sql.Conn) error {
	select {
	case r.reset <- struct{}{}:
	default:
	}
	return nil
}

func TestTenancyConn_TenantComesOnlyFromAClaim(t *testing.T) {
	// Without a database, the part this can prove is the tenant each request
	// resolves: a request carrying a tenant claim resolves it, and an
	// anonymous one resolves none, so it has nothing to scope a connection to.
	var tenanted, untenanted bool

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mw.TenantIDFromCtx(r) != "" {
			tenanted = true
		} else {
			untenanted = true
		}
	})
	m := mw.TenantHeader(true)(mw.TenancyConn(nil, recordingTenancy{reset: make(chan struct{}, 1)})(next))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
	req = req.WithContext(context.WithValue(req.Context(),
		auth.ClaimsKey, &auth.Claims{TenantID: "tenant-a"}))
	m.ServeHTTP(httptest.NewRecorder(), req)

	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/content", nil))

	if !tenanted {
		t.Error("a request carrying a tenant claim resolved no tenant")
	}
	if !untenanted {
		t.Error("a request with no claim resolved a tenant")
	}
}

func TestTenantConn_RoundTrip(t *testing.T) {
	// WithTenantConn and TenantConn are the plumbing that carries the
	// per-request connection.
	ctx := db.WithTenantConn(context.Background(), nil)
	if db.TenantConn(ctx) != nil {
		t.Error("TenantConn(WithTenantConn(nil)) should return nil")
	}

	// Verify unrelated context values don't interfere.
	type otherKey struct{}
	ctx2 := context.WithValue(context.Background(), otherKey{}, "garbage")
	if db.TenantConn(ctx2) != nil {
		t.Error("TenantConn should not pick up unrelated context values")
	}
}

// Tenant existence
//
// A caller must not learn whether a tenant exists from how a request is
// answered: not from its timing, and not from its error.

func TestTenantHeader_AnonymousHeaderTakesTheFastPath(t *testing.T) {
	// Without credentials TenantHeader resolves nothing from X-Tenant-ID, so
	// every anonymous probe takes the same short path whatever slug it names.
	m := mw.TenantHeader(true)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
		req.Header.Set("X-Tenant-ID", "probe_tenant_"+string(rune('a'+i%26)))
		rec := httptest.NewRecorder()
		start := time.Now()
		m(next).ServeHTTP(rec, req)
		elapsed := time.Since(start)

		// All should be sub-millisecond since TenantHeader is a no-op
		// without JWT claims.
		if elapsed > 10*time.Millisecond {
			t.Errorf("TenantHeader no-op took %v, an unexpected slowdown", elapsed)
		}
	}
}

func TestTenantHeader_UnknownTenantErrorIsGeneric(t *testing.T) {
	// An error must not distinguish "this tenant does not exist" from "this
	// tenant exists and is not yours". The super-admin header path is the one
	// place the engine answers a tenant-existence question at all, and it
	// answers 404 either way.
	m := mw.TenantHeader(true, func(context.Context, string) bool { return false })

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, slug := range []string{"does_not_exist", "exists_elsewhere"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/content", nil)
		req.Header.Set("X-Tenant-ID", slug)
		req = req.WithContext(context.WithValue(req.Context(),
			auth.ClaimsKey, &auth.Claims{TenantID: "home", Roles: []string{"super_admin"}}))

		rec := httptest.NewRecorder()
		m(next).ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("slug %q: status %d; want 404 for every unvalidated tenant", slug, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "tenant not found") || strings.Contains(body, slug) {
			t.Errorf("slug %q: body %q must be generic and must not echo the slug", slug, body)
		}
	}
}

// Single-tenant installs and archived tenants

func TestTenantHeader_SingleTenantUsesTheJWTTenantClaim(t *testing.T) {
	// With multiTenant=false TenantHeader still takes the JWT tenant claim, so
	// stores that call RequireTenantID have a tenant. On a single-tenant
	// install the claim is informational: no database isolation applies to it.
	m := mw.TenantHeader(false)
	var resolvedTenant string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolvedTenant = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	claims := &auth.Claims{TenantID: "from-the-claim", Roles: []string{"super_admin"}}
	ctx := context.WithValue(req.Context(), auth.ClaimsKey, claims)
	req = req.WithContext(ctx)

	m(next).ServeHTTP(httptest.NewRecorder(), req)

	if resolvedTenant != "from-the-claim" {
		t.Errorf("multiTenant=false: expected tenant from JWT claims, got %q", resolvedTenant)
	}
}

func TestReadOnlyArchived_PassesThroughWithoutATenantOrChecker(t *testing.T) {
	// The ReadOnlyArchived middleware blocks mutating requests against archived
	// tenants with HTTP 423. GET, HEAD and OPTIONS always pass through, and a
	// mutating request passes through when there is no tenant to check or no
	// checker to ask. The tenant is read from an unexported context key, so a
	// request that names one needs the full TenantHeader pipeline.

	t.Run("no-tenant_passes_through", func(t *testing.T) {
		checker := func(ctx context.Context, slug string) (bool, bool) {
			return true, true // would be archived if tenant existed
		}
		m := mw.ReadOnlyArchived(checker)
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/content", nil)
		rec := httptest.NewRecorder()
		m(next).ServeHTTP(rec, req)

		if !called {
			t.Error("POST with no tenant should pass through")
		}
	})

	t.Run("nil-checker_passes_through", func(t *testing.T) {
		m := mw.ReadOnlyArchived(nil)
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})

		req := httptest.NewRequest(http.MethodPost, "/api/v1/content", nil)
		rec := httptest.NewRecorder()
		m(next).ServeHTTP(rec, req)

		if !called {
			t.Error("nil checker should be no-op")
		}
	})
}

// Helpers

// sanitizeName converts a test case name to something safe for t.Run.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, s)
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}
