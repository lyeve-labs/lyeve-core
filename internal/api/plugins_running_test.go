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
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// tenantRefusals is a licensing implementation's manager reduced to what
// each tenant is refused.
type tenantRefusals map[string][]string

func (tenantRefusals) Snapshot() licensing.Snapshot { return unlicensedEntitlements{}.Snapshot() }
func (t tenantRefusals) Withholds(tenant, name string) bool {
	for _, n := range t[tenant] {
		if n == name {
			return true
		}
	}
	return false
}
func (t tenantRefusals) WithheldFrom(tenant string) []string {
	return append([]string{}, t[tenant]...)
}
func (tenantRefusals) Plugin(string) licensing.PluginGrant { return licensing.PluginGrant{} }

// runningReport has a plugin in every phase, a plugin the build does not
// compile, and a plugin the license refuses.
func runningReport() stubPluginStatusProvider {
	return stubPluginStatusProvider{report: plugin.PluginStatusReport{Plugins: []plugin.PluginStatus{
		{Name: "search", Compiled: true, Entitled: true, Requested: true, Active: true, Phase: plugin.PhaseRunning},
		{Name: "content", Compiled: true, Entitled: true, Requested: true, Active: true, Phase: plugin.PhaseRunning},
		{Name: "media", Compiled: true, Entitled: true, Requested: true, Active: true, Phase: plugin.PhaseRunning},
		{Name: "reports", Compiled: true, Entitled: true, Requested: true, Phase: plugin.PhaseLazy},
		{Name: "broken", Compiled: true, Entitled: true, Requested: true, Phase: plugin.PhaseFailed, LastError: "dial tcp: connection refused"},
		{Name: "booting", Compiled: true, Entitled: true, Requested: true, Phase: plugin.PhaseStarting},
		{Name: "stopped", Compiled: true, Entitled: true, Requested: true, Phase: plugin.PhaseStopped},
		{Name: "refused", Compiled: true, Phase: plugin.PhaseRegistered, Reason: "not granted", UpgradeURL: "https://example.com/license"},
		{Name: "ghost", Requested: true, Phase: plugin.PhaseRegistered, Reason: "plugin not compiled into this build"},
	}}}
}

// runningFor answers the handler for a request in tenant.
func runningFor(t *testing.T, provider PluginStatusProvider, ent EntitlementProvider, tenant string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/running", nil)
	req = req.WithContext(core.WithTenantID(req.Context(), tenant))
	rec := httptest.NewRecorder()
	runningPluginsHandler(provider, ent)(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec
}

// The plugins that run or start on first use serve the tenant unless the
// tenant is refused them, and the ones it is refused are listed apart. A
// plugin that failed, is still starting, stopped, was refused by the license
// or is not compiled serves nobody.
func TestRunningPluginsHandler_NamesWhatServesTheCallersTenant(t *testing.T) {
	refusals := tenantRefusals{"acme": {"search", "reports", "refused", "unknown"}}

	rec := runningFor(t, runningReport(), refusals, "acme")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"plugins":["content","media"],"withheld":["reports","search"]}`, rec.Body.String())

	other := runningFor(t, runningReport(), refusals, "other")
	assert.JSONEq(t, `{"plugins":["content","media","reports","search"],"withheld":[]}`, other.Body.String())
}

// Every signed-in role reads the body, so it names plugins and nothing an
// operator reads about them.
func TestRunningPluginsHandler_CarriesNamesOnly(t *testing.T) {
	body := runningFor(t, runningReport(), tenantRefusals{}, "acme").Body.String()
	var members map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &members))
	assert.Len(t, members, 2)
	assert.Contains(t, members, "plugins")
	assert.Contains(t, members, "withheld")
	for _, detail := range []string{"connection refused", "not granted", "https://", "not compiled"} {
		assert.NotContains(t, body, detail)
	}
}

func TestRunningPluginsHandler_NeverAnswersNull(t *testing.T) {
	for name, provider := range map[string]PluginStatusProvider{
		"no status provider": nil,
		"an empty report":    stubPluginStatusProvider{},
	} {
		t.Run(name, func(t *testing.T) {
			rec := runningFor(t, provider, unlicensedEntitlements{}, "acme")
			assert.JSONEq(t, `{"plugins":[],"withheld":[]}`, rec.Body.String())
		})
	}
}

// signedInAs is a signed session for a user of tenant holding roles, at
// token version 0 so the version check reads the stub row.
func signedInAs(t *testing.T, tenant string, roles ...string) string {
	t.Helper()
	tok, err := auth.Sign(testConfig().JWTSecret, 3600, uuid.New(), "someone@test.local", roles, tenant, 0)
	require.NoError(t, err)
	return tok
}

// keyHolding authenticates every X-API-Key as one declared key with the
// roles and scopes given.
func keyHolding(roles, scopes []string) RouterOption {
	return WithAPIKeyAuth(apimw.APIKeyAuth(func(context.Context, string) (*core.AuthClaims, error) {
		return &core.AuthClaims{UserID: "declared-key", Roles: roles, Scopes: scopes, IsAPIKey: true, APIKeyID: "declared-key"}, nil
	}))
}

// On the admin router every signed-in role reads the running plugins, the
// tenant it acts in decides what is withheld, and a caller with no session
// is refused.
func TestRunningPlugins_EverySignedInRoleOnTheAdminRouter(t *testing.T) {
	h, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(),
		WithLifetime(testLifetime(t)), WithPluginStatus(runningReport()), WithEntitlements(tenantRefusals{"acme": {"search"}}))
	require.NoError(t, err)

	get := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/running", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, role := range []string{"editor", "admin", "super_admin"} {
		rec := get(signedInAs(t, "acme", role))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", role, rec.Body.String())
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.JSONEq(t, `{"plugins":["content","media","reports"],"withheld":["search"]}`, rec.Body.String(), role)
	}
	assert.JSONEq(t, `{"plugins":["content","media","reports","search"],"withheld":[]}`, get(signedInAs(t, "", "editor")).Body.String(),
		"a session in another tenant is refused nothing")
	assert.Equal(t, http.StatusUnauthorized, get("").Code)
}

// On the stateless router a declared key reads the running plugins on the
// terms of any authenticated route there: an admin role, or a scope that
// covers plugins.
func TestRunningPlugins_DeclaredKeysOnTheStatelessRouter(t *testing.T) {
	get := func(key RouterOption, sendKey bool) *httptest.ResponseRecorder {
		h := NewStatelessRouter(testConfig(), NewStatelessMode(nil),
			WithLifetime(testLifetime(t)), WithPluginStatus(runningReport()), key)
		req := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/running", nil)
		if sendKey {
			req.Header.Set("X-API-Key", "any-declared-key")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	admin := get(keyHolding([]string{"admin"}, nil), true)
	require.Equal(t, http.StatusOK, admin.Code, admin.Body.String())
	assert.JSONEq(t, `{"plugins":["content","media","reports","search"],"withheld":[]}`, admin.Body.String())

	scoped := get(keyHolding(nil, []string{"plugins:read"}), true)
	assert.Equal(t, http.StatusOK, scoped.Code, "a key whose scopes cover plugins needs no role: %s", scoped.Body.String())

	assert.Equal(t, http.StatusForbidden, get(keyHolding(nil, []string{"content:read"}), true).Code, "a scoped key is held to its scopes")
	assert.Equal(t, http.StatusUnauthorized, get(keyHolding([]string{"admin"}, nil), false).Code, "no key, no answer")
}

// The document lists the route with no role requirement, since every
// signed-in role reads it.
func TestRunningPluginsSpec_NeedsNoRole(t *testing.T) {
	op := buildOpenAPIDoc(nil, nil, openAPIOptions{}).Paths["/api/admin/plugins/running"]["get"]
	require.NotNil(t, op)
	assert.NotEmpty(t, op.Security)
	assert.Nil(t, op.Extensions[xRoles])
	assert.Contains(t, op.Description, "withheld")
}
