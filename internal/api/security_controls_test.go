package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubControls struct{ rows []core.SecurityControl }

func (s stubControls) SecurityControls() []core.SecurityControl { return s.rows }

// Each row carries the remedy its plugin sends, the passing one included, and
// the report shows it only where the control is not enforcing.
func TestSecurityControlsHandler_ReportsRemediesForWhatIsNotEnforcing(t *testing.T) {
	t.Parallel()

	provider := stubControls{rows: []core.SecurityControl{
		{Control: "waf", Status: core.ControlPass, Detail: "waf middleware active",
			Remedy: "Enable a plugin that supplies a web application firewall."},
		{Control: "quota/rate-limit", Status: core.ControlSkip, Detail: "no rate limiting configured",
			Remedy: "Set RATE_LIMIT_RPS above 0 for the built-in per-IP limiter, or enable a plugin that supplies rate limiting."},
		{Control: "mfa", Status: core.ControlFail, Detail: "MFA store not wired into auth handler",
			Remedy: "Enable a plugin that supplies an MFA store."},
	}}

	w := httptest.NewRecorder()
	securityControlsHandler(provider).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/security/controls", nil))

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	var body securityControlsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Controls, 3)
	assert.False(t, body.CheckedAt.IsZero())
	assert.Equal(t, 1, body.Failures)

	// A passing control needs nothing done. The others say what turns them on.
	assert.Empty(t, body.Controls[0].Remedy)
	assert.Contains(t, body.Controls[1].Remedy, "RATE_LIMIT_RPS")
	assert.Contains(t, body.Controls[2].Remedy, "MFA store")
}

// A plugin that reports its own control names the remedy for it, and the
// report keeps that remedy rather than the engine's. A passing row carries
// none, whoever reported it.
func TestSecurityControlsHandler_KeepsTheRemedyARowCarries(t *testing.T) {
	t.Parallel()

	provider := stubControls{rows: []core.SecurityControl{
		{Control: "mfa", Status: core.ControlWarn, Detail: "no factor enrolled", Remedy: "Enroll a second factor for every admin."},
		{Control: "honeypot", Status: core.ControlPass, Detail: "trap routes armed", Remedy: "Arm the trap routes."},
		{Control: "honeypot-alerts", Status: core.ControlFail, Detail: "no alert channel"},
	}}

	w := httptest.NewRecorder()
	securityControlsHandler(provider).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/security/controls", nil))
	require.Equal(t, http.StatusOK, w.Code)

	var body securityControlsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Controls, 3)
	assert.Equal(t, "Enroll a second factor for every admin.", body.Controls[0].Remedy)
	assert.Empty(t, body.Controls[1].Remedy)
	assert.Empty(t, body.Controls[2].Remedy, "a control the engine has no remedy for shows none")
	assert.Equal(t, 1, body.Failures)
}

func TestSecurityControlsHandler_NoRuntimeIsNotFound(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	securityControlsHandler(nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/security/controls", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAdminRouter_RobotsRefusesEveryCrawlerWithoutAuth(t *testing.T) {
	t.Parallel()

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)))
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	require.Equal(t, http.StatusOK, w.Code, "body=%q", w.Body.String())
	assert.Equal(t, "User-agent: *\nDisallow: /\n", w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")
	assert.Equal(t, "noindex, nofollow, noarchive", w.Header().Get("X-Robots-Tag"))
}

func TestAdminRouter_SecurityControlsNeedASession(t *testing.T) {
	t.Parallel()

	router, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), WithLifetime(testLifetime(t)),
		WithSecurityControls(stubControls{}))
	require.NoError(t, err)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/admin/security/controls", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
