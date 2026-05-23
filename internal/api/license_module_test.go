package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// bearerWithRoles is a signed session for a user holding roles, at token
// version 0 so the version check reads the stub row.
func bearerWithRoles(t *testing.T, roles ...string) string {
	t.Helper()
	tok, err := auth.Sign(testConfig().JWTSecret, 3600, uuid.New(), "someone@test.local", roles, "", 0)
	require.NoError(t, err)
	return tok
}

// declaredKeyWithRoles authenticates every X-API-Key as one key holding
// roles, the way a declared key reaches the stateless router.
func declaredKeyWithRoles(roles ...string) RouterOption {
	return WithAPIKeyAuth(apimw.APIKeyAuth(func(context.Context, string) (*core.AuthClaims, error) {
		return &core.AuthClaims{UserID: "declared-key", Roles: roles, IsAPIKey: true, APIKeyID: "declared-key"}, nil
	}))
}

// entitlementsJSON calls the handler and returns the body's members.
func entitlementsJSON(t *testing.T, ent EntitlementProvider, licenseModule bool) map[string]json.RawMessage {
	t.Helper()
	rr := httptest.NewRecorder()
	entitlementsHandler(ent, licenseModule)(rr, httptest.NewRequest(http.MethodGet, "/api/admin/entitlements", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	return body
}

// license_module is sent whatever its value, so an engine too old to send it
// is told apart by its absence.
func TestEntitlementsHandler_AlwaysSaysWhetherALicenseModuleIsLinked(t *testing.T) {
	open, err := licensing.Open().NewManager(context.Background(), licensing.Env{Names: []string{"widgets"}})
	require.NoError(t, err)

	assert.JSONEq(t, `false`, string(entitlementsJSON(t, open, false)["license_module"]))
	assert.JSONEq(t, `true`, string(entitlementsJSON(t, unlicensedEntitlements{}, true)["license_module"]))
}

// The runtime's answer reaches the body on the admin router and on the
// stateless one, and a router nobody told answers false.
func TestEntitlements_LicenseModuleOnBothRouters(t *testing.T) {
	type routerCase struct {
		name string
		h    http.Handler
		want string
	}
	newAdmin := func(opts ...RouterOption) http.Handler {
		h, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(),
			append([]RouterOption{WithLifetime(testLifetime(t))}, opts...)...)
		require.NoError(t, err)
		return h
	}
	newStateless := func(opts ...RouterOption) http.Handler {
		return NewStatelessRouter(testConfig(), NewStatelessMode(nil),
			append([]RouterOption{WithLifetime(testLifetime(t)), declaredKeyWithRoles("admin")}, opts...)...)
	}
	cases := []routerCase{
		{"admin, linked", newAdmin(WithLicenseModule(true)), "true"},
		{"admin, not linked", newAdmin(WithLicenseModule(false)), "false"},
		{"admin, never told", newAdmin(), "false"},
		{"stateless, linked", newStateless(WithLicenseModule(true)), "true"},
		{"stateless, never told", newStateless(), "false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/entitlements", nil)
			req.Header.Set("Authorization", "Bearer "+bearerWithRoles(t, "admin"))
			req.Header.Set("X-API-Key", "any-declared-key")
			rec := httptest.NewRecorder()
			c.h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.JSONEq(t, c.want, string(body["license_module"]))
		})
	}
}

// The entitlements operation names the fields a licensing implementation may
// add and the one the engine always sends.
func TestEntitlementsSpec_NamesTheLicenseModuleAndTheOptionalFields(t *testing.T) {
	op := buildOpenAPIDoc(nil, nil, openAPIOptions{}).Paths["/api/admin/entitlements"]["get"]
	require.NotNil(t, op)
	for _, field := range []string{"plan_label", "grace_ends_at", "license_module"} {
		assert.Contains(t, op.Description, field)
	}
}
