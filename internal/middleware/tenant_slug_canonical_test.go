package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A tenant has one spelling. MySQL and MSSQL collate their string columns case
// insensitively, so a roster lookup for `Acme` answers with the row for `acme`
// and the request then runs under the name the caller sent: the isolation
// predicate, the audit record and every structure keyed by tenant carry a name
// the engine never issued, and the same request behaves differently on
// PostgreSQL. The shape is checked before the lookup so the three dialects
// agree.
func TestTenantHeader_OverrideMustBeCanonical(t *testing.T) {
	// The roster folds case, which is what a MySQL or MSSQL column does with
	// its default collation. A Go comparison would hide the whole defect: it
	// is the database that answers `Acme` with the row for `acme`.
	roster := func(_ context.Context, slug string) bool {
		return strings.EqualFold(slug, "acme")
	}

	cases := []struct {
		name   string
		header string
		want   string
		status int
	}{
		{"the canonical slug resolves", "acme", "acme", http.StatusOK},
		{"a mixed-case spelling is not that tenant", "Acme", "", http.StatusNotFound},
		{"upper case is not that tenant", "ACME", "", http.StatusNotFound},
		{"a hyphen is not a slug character", "acme-ops", "", http.StatusNotFound},
		{"a slug cannot start with a digit", "1acme", "", http.StatusNotFound},
		{"nor with an underscore", "_acme", "", http.StatusNotFound},
		{"a quote never reaches the roster", "acme'; DROP TABLE sys_tenants--", "", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = mw.TenantIDFromCtx(r)
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Tenant-ID", tc.header)
			claims := &auth.Claims{Roles: []string{"super_admin"}}
			req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

			rec := httptest.NewRecorder()
			mw.TenantHeader(true, core.TenantValidatorFunc(roster))(next).ServeHTTP(rec, req)

			if rec.Code != tc.status {
				t.Fatalf("%q: got %d, want %d", tc.header, rec.Code, tc.status)
			}
			if got != tc.want {
				t.Fatalf("%q resolved to %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// The shape check does not need a roster to run. An install without the tenant
// plugin registers no validator, and a slug the engine could never have issued
// is still not a tenant there.
func TestTenantHeader_OverrideMustBeCanonicalWithoutARoster(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = mw.TenantIDFromCtx(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Tenant-ID", "Acme")
	claims := &auth.Claims{Roles: []string{"super_admin"}}
	req = req.WithContext(context.WithValue(req.Context(), auth.ClaimsKey, claims))

	rec := httptest.NewRecorder()
	mw.TenantHeader(true)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if got != "" {
		t.Fatalf("resolved to %q, want no tenant", got)
	}
}

func TestIsValidTenantSlug(t *testing.T) {
	valid := []string{"a", "acme", "acme_ops", "t1", core.DefaultTenantSlug}
	for _, s := range valid {
		if !core.IsValidTenantSlug(s) {
			t.Errorf("%q must be a valid slug", s)
		}
	}
	invalid := []string{"", "A", "Acme", "acme-ops", "1acme", "_acme", "acme ops",
		"acme.ops", "acme\"", "acme`", "acme]", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, s := range invalid {
		if core.IsValidTenantSlug(s) {
			t.Errorf("%q must be refused", s)
		}
	}
}
