package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

const setupModeTestToken = "setup-mode-token-0123456789"

var (
	missingDB      = config.SetupSetting{EnvKey: "DATABASE_URL", YAMLPath: "database.url"}
	missingJWT     = config.SetupSetting{EnvKey: "JWT_SECRET", YAMLPath: "jwt.secret", Secret: true}
	missingEncrypt = config.SetupSetting{EnvKey: "ENCRYPTION_KEY", YAMLPath: "encryption.key", Secret: true}
)

func serveSetupMode(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set(SetupTokenHeader, token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSetupModeHandler_ServesOnlyProbesAndSetup(t *testing.T) {
	t.Parallel()
	h := NewSetupModeHandler([]config.SetupSetting{missingDB}, NewSetupTokenFromEnv(setupModeTestToken), nil)

	cases := []struct {
		method, path string
		want         int
		contains     string
	}{
		{http.MethodGet, "/healthz", http.StatusOK, `"mode":"setup"`},
		{http.MethodGet, "/readyz", http.StatusServiceUnavailable, `"not_ready"`},
		{http.MethodGet, "/api/admin/setup", http.StatusOK, `"token_source":"env"`},
		{http.MethodGet, "/api/admin/schemas", http.StatusServiceUnavailable, "SETUP_REQUIRED"},
		{http.MethodGet, "/api/v1/articles", http.StatusServiceUnavailable, "SETUP_REQUIRED"},
		{http.MethodPost, "/api/admin/setup", http.StatusServiceUnavailable, "SETUP_REQUIRED"},
		{http.MethodPost, "/api/admin/auth/login", http.StatusServiceUnavailable, "SETUP_REQUIRED"},
	}
	for _, tc := range cases {
		rr := serveSetupMode(h, tc.method, tc.path, "")
		assert.Equal(t, tc.want, rr.Code, "%s %s", tc.method, tc.path)
		assert.Contains(t, rr.Body.String(), tc.contains, "%s %s", tc.method, tc.path)
		assert.Equal(t, "no-store", rr.Header().Get("Cache-Control"), "%s %s", tc.method, tc.path)
	}
}

func TestSetupModeHandler_StatusRequiresToken(t *testing.T) {
	t.Parallel()
	h := NewSetupModeHandler([]config.SetupSetting{missingJWT}, NewSetupTokenFromEnv(setupModeTestToken),
		map[string]string{"JWT_SECRET": "generated-jwt-secret"})

	assert.Equal(t, http.StatusUnauthorized, serveSetupMode(h, http.MethodGet, "/api/admin/setup/status", "").Code)
	wrong := serveSetupMode(h, http.MethodGet, "/api/admin/setup/status", "setup-mode-token-guess")
	assert.Equal(t, http.StatusUnauthorized, wrong.Code)
	assert.NotContains(t, wrong.Body.String(), "generated-jwt-secret")

	// A refused read must not spend the one showing.
	ok := serveSetupMode(h, http.MethodGet, "/api/admin/setup/status", setupModeTestToken)
	require.Equal(t, http.StatusOK, ok.Code)
	assert.Contains(t, ok.Body.String(), "generated-jwt-secret")
}

func TestSetupModeHandler_StatusShowsGeneratedSecretsOnce(t *testing.T) {
	t.Parallel()
	h := NewSetupModeHandler([]config.SetupSetting{missingDB, missingJWT, missingEncrypt},
		NewSetupTokenFromEnv(setupModeTestToken),
		map[string]string{"JWT_SECRET": "generated-jwt-secret", "ENCRYPTION_KEY": "generated-encryption-key"})

	read := func() SetupModeStatus {
		t.Helper()
		rr := serveSetupMode(h, http.MethodGet, "/api/admin/setup/status", setupModeTestToken)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var s SetupModeStatus
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &s))
		return s
	}

	first := read()
	assert.Equal(t, "setup", first.Mode)
	assert.True(t, first.SecretsIncluded)
	assert.Equal(t, []SetupMissingSetting{
		{Env: "DATABASE_URL", YAML: "database.url"},
		{Env: "JWT_SECRET", YAML: "jwt.secret", Generated: true},
		{Env: "ENCRYPTION_KEY", YAML: "encryption.key", Generated: true},
	}, first.Missing)
	assert.Equal(t, "DATABASE_URL="+databaseURLPlaceholder+"\nJWT_SECRET=generated-jwt-secret\nENCRYPTION_KEY=generated-encryption-key\n", first.Env)
	assert.Equal(t, "database:\n  url: \""+databaseURLPlaceholder+"\"\njwt:\n  secret: \"generated-jwt-secret\"\nencryption:\n  key: \"generated-encryption-key\"\n", first.YAML)
	assert.NotEmpty(t, first.Restart)

	second := read()
	assert.False(t, second.SecretsIncluded)
	assert.NotContains(t, second.Env, "generated-jwt-secret")
	assert.NotContains(t, second.YAML, "generated-encryption-key")
	assert.Contains(t, second.Env, "JWT_SECRET="+secretShownPlaceholder)
}

func TestSetupModeHandler_DatabaseOnlyCarriesNoSecrets(t *testing.T) {
	t.Parallel()
	h := NewSetupModeHandler([]config.SetupSetting{missingDB}, NewSetupTokenFromEnv(setupModeTestToken), map[string]string{})
	rr := serveSetupMode(h, http.MethodGet, "/api/admin/setup/status", setupModeTestToken)
	require.Equal(t, http.StatusOK, rr.Code)
	var s SetupModeStatus
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &s))
	assert.False(t, s.SecretsIncluded)
	assert.Equal(t, "DATABASE_URL="+databaseURLPlaceholder+"\n", s.Env)
	assert.True(t, strings.HasPrefix(s.YAML, "database:\n"))
}

func TestSetupModeHandler_NoTokenRefusesStatus(t *testing.T) {
	t.Parallel()
	h := NewSetupModeHandler([]config.SetupSetting{missingDB}, NewSetupTokenFromEnv(""), nil)
	assert.Equal(t, http.StatusUnauthorized, serveSetupMode(h, http.MethodGet, "/api/admin/setup/status", "").Code)
	rr := serveSetupMode(h, http.MethodGet, "/api/admin/setup", "")
	assert.NotContains(t, rr.Body.String(), "token_source")
}
