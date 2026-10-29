package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/testhost"
	"github.com/lyeve-labs/lyeve-core/internal/testsupply/schemaengine"
)

// A schema definition written under no tenant is listed and owned by no
// tenant, so a schema write that names no tenant is refused. On a multi-tenant
// install a super admin's request names one through its claim, X-Tenant-ID or
// a mapped domain, or it names none, and the import refuses the last before it
// reads the upload, whether asked to plan or to apply. A single-tenant install
// resolves every request to its one tenant and imports as before.
func TestSchemaImport_RequiresATenant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}

	forEachImportDialect(t, func(t *testing.T, rig *importRig) {
		t.Run("a multi-tenant request naming no tenant is refused unread", func(t *testing.T) {
			h, eng := rig.tenantRouter(t, true)
			for _, query := range []string{"", "?apply=true"} {
				src := &countingReader{r: bytes.NewReader(tenantBundle("unscoped"))}
				req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas/import"+query, src)
				req.Header.Set("Content-Type", "application/yaml")
				req.Header.Set("Authorization", "Bearer "+rig.sessionFor(t, ""))
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				require.Equal(t, http.StatusBadRequest, rec.Code, "query %q: %s", query, rec.Body.String())
				msg, code := refusalOf(t, rec)
				assert.Equal(t, "schema definitions belong to a tenant, and the request names none", msg)
				assert.Equal(t, "bad_request", code)
				assert.Zero(t, src.n, "query %q: bytes read from a body already refused", query)
			}
			defs, err := eng.List(context.Background())
			require.NoError(t, err)
			assert.Empty(t, defs, "a refused import writes nothing")
		})

		scoped := []struct {
			name, tenant, header, host string
		}{
			{"a claim names the tenant", "acme", "", ""},
			{"X-Tenant-ID names the tenant", "", "acme", ""},
			{"a mapped domain names the tenant", "", "", "acme.example.com"},
		}
		for i, tc := range scoped {
			t.Run(tc.name, func(t *testing.T) {
				h, _ := rig.tenantRouter(t, true)
				schema := fmt.Sprintf("scoped_%d", i)
				assertImported(t, rig.importAs(t, h, "", tc.tenant, tc.header, tc.host, schema), false)
				assertImported(t, rig.importAs(t, h, "?apply=true", tc.tenant, tc.header, tc.host, schema), true)
			})
		}

		t.Run("a single-tenant request imports under the implicit tenant", func(t *testing.T) {
			h, _ := rig.tenantRouter(t, false)
			assertImported(t, rig.importAs(t, h, "", "", "", "", "single"), false)
			assertImported(t, rig.importAs(t, h, "?apply=true", "", "", "", "single"), true)
		})
	})
}

// tenantRouter builds the admin router over the rig's database, multi-tenant
// or not, with hostRouting mapping acme.example.com to the acme tenant.
func (rg *importRig) tenantRouter(t *testing.T, multiTenant bool) (http.Handler, *schemaengine.Engine) {
	t.Helper()
	cfg := testConfig()
	cfg.MultiTenant = multiTenant
	host := testhost.New(rg.pool)
	eng := schemaengine.New(host)
	h, err := NewAdminRouter(rg.pool, cfg,
		WithLifetime(testLifetime(t)),
		WithPreAuthMiddleware(hostRouting),
		WithScalingHost(importHost{Host: host, eng: eng}))
	require.NoError(t, err)
	return h, eng
}

// sessionFor signs the rig's super admin in with tenant as its claim.
func (rg *importRig) sessionFor(t *testing.T, tenant string) string {
	t.Helper()
	u := rg.user
	session, err := auth.Sign(testConfig().JWTSecret, 3600, u.ID, u.Email, u.Roles, tenant, u.TokenVersion)
	require.NoError(t, err)
	return session
}

// importAs posts a one-type bundle to the import as the rig's super admin,
// with tenant as the claim and header and host set when not empty.
func (rg *importRig) importAs(t *testing.T, h http.Handler, query, tenant, header, host, schema string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/schemas/import"+query, bytes.NewReader(tenantBundle(schema)))
	req.Header.Set("Content-Type", "application/yaml")
	req.Header.Set("Authorization", "Bearer "+rg.sessionFor(t, tenant))
	if header != "" {
		req.Header.Set("X-Tenant-ID", header)
	}
	if host != "" {
		req.Host = host
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// tenantBundle is a bundle of one content type named schema.
func tenantBundle(schema string) []byte {
	return []byte("schemas:\n  - name: " + schema + "\n    fields: [{name: title, field_type: text}]\n")
}

// assertImported checks that the import planned the one new content type and,
// when asked to, applied it.
func assertImported(t *testing.T, rec *httptest.ResponseRecorder, applied bool) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out schemaImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, applied, out.Applied)
	assert.Len(t, out.Plan.Schemas, 1)
}
