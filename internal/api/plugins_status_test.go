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
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// keyWithRoles authenticates every X-API-Key as one key holding roles, the
// way a declared key reaches the stateless router.
func keyWithRoles(roles ...string) RouterOption {
	return WithAPIKeyAuth(apimw.APIKeyAuth(func(context.Context, string) (*core.AuthClaims, error) {
		return &core.AuthClaims{UserID: "declared-key", Roles: roles, IsAPIKey: true, APIKeyID: "declared-key"}, nil
	}))
}

// sessionBearer is a signed session for a user holding roles, at token
// version 0 so the version check reads the stub row.
func sessionBearer(t *testing.T, roles ...string) string {
	t.Helper()
	tok, err := auth.Sign(testConfig().JWTSecret, 3600, uuid.New(), "someone@test.local", roles, "", 0)
	require.NoError(t, err)
	return tok
}

// A row carries the manifest its plugin gave, and a row whose plugin gave
// none leaves the key out, on the admin router and on the stateless one.
func TestPluginsStatus_CarriesTheManifestOnBothRouters(t *testing.T) {
	status := stubPluginStatusProvider{report: plugin.PluginStatusReport{
		Compiled: []string{"plain", "search"},
		Entitled: []string{"plain", "search"},
		Plugins: []plugin.PluginStatus{
			{Name: "plain", Compiled: true, Entitled: true, Requested: true, Active: true, Phase: plugin.PhaseRunning},
			{Name: "search", Compiled: true, Entitled: true, Requested: true, Active: true, Phase: plugin.PhaseRunning,
				Manifest: &core.PluginManifest{Label: "Search", Description: "Ranked full-text search across every schema.", Category: core.CategoryContent, Maturity: core.MaturityStable}},
		},
	}}

	admin, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(),
		WithLifetime(testLifetime(t)), WithPluginStatus(status))
	require.NoError(t, err)
	stateless := NewStatelessRouter(testConfig(), NewStatelessMode(nil),
		WithLifetime(testLifetime(t)), WithPluginStatus(status), keyWithRoles("admin"))

	adminReq := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/status", nil)
	adminReq.Header.Set("Authorization", "Bearer "+sessionBearer(t, "admin"))
	statelessReq := httptest.NewRequest(http.MethodGet, "/api/admin/plugins/status", nil)
	statelessReq.Header.Set("X-API-Key", "any-declared-key")

	for name, c := range map[string]struct {
		h   http.Handler
		req *http.Request
	}{"admin": {admin, adminReq}, "stateless": {stateless, statelessReq}} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.h.ServeHTTP(rec, c.req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			var body struct {
				Plugins []map[string]json.RawMessage `json:"plugins"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Len(t, body.Plugins, 2)
			_, plainHas := body.Plugins[0]["manifest"]
			assert.False(t, plainHas, "a plugin that does not describe itself has no manifest key")
			assert.JSONEq(t,
				`{"label":"Search","description":"Ranked full-text search across every schema.","category":"content","maturity":"stable"}`,
				string(body.Plugins[1]["manifest"]))
		})
	}
}

// The document says what a manifest holds, with the limits and the
// categories the engine applies.
func TestPluginsStatusSpec_DescribesTheManifest(t *testing.T) {
	op := buildOpenAPIDoc(nil, nil, openAPIOptions{}).Paths["/api/admin/plugins/status"]["get"]
	require.NotNil(t, op)
	assert.Contains(t, op.Description, "manifest")
	assert.Contains(t, op.Description, "a label of at most 40 characters")
	assert.Contains(t, op.Description, "a description of at most 240")
	for _, c := range core.PluginCategories() {
		assert.Contains(t, op.Description, string(c))
	}
}
