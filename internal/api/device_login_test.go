package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// deviceStart is the body a device gets back when it starts a sign-in.
type deviceStart struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// startDevice starts a sign-in from remote and returns the codes.
func (rg *tokenRig) startDevice(remote string) deviceStart {
	rg.t.Helper()
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli on build-box"}, remote: remote})
	require.Equal(rg.t, http.StatusOK, rec.Code, rec.Body.String())
	var out deviceStart
	require.NoError(rg.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// pollDevice polls once and returns the status and the decoded body.
func (rg *tokenRig) pollDevice(deviceCode, remote string) (int, map[string]any) {
	rg.t.Helper()
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/token", body: map[string]any{"device_code": deviceCode}, remote: remote})
	var out map[string]any
	require.NoError(rg.t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return rec.Code, out
}

func (rg *tokenRig) decide(userCode, action, bearer string) (int, map[string]any) {
	rg.t.Helper()
	method, path := http.MethodPost, "/api/admin/auth/device/"+userCode+"/"+action
	if action == "" {
		method, path = http.MethodGet, "/api/admin/auth/device/"+userCode
	}
	body := map[string]any{}
	if action == "approve" {
		body["password"] = tokenTestPassword
	}
	rec := rg.do(tokenCall{method: method, path: path, bearer: bearer, body: body})
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// approveWith approves with a chosen step-up body.
func (rg *tokenRig) approveWith(userCode, bearer string, body map[string]any) *httptest.ResponseRecorder {
	rg.t.Helper()
	return rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/" + userCode + "/approve", bearer: bearer, body: body})
}

var userCodeShape = regexp.MustCompile(`^[ABCDEFGHJKMNPQRSTUVWXYZ2-9]{4}-[ABCDEFGHJKMNPQRSTUVWXYZ2-9]{4}$`)

// auditOnlyHost stands in for the engine host where only the audit writer,
// which never calls it, is reached.
type auditOnlyHost struct{ core.Host }

// recordingAuditWriter keeps the audit entries written through it.
type recordingAuditWriter struct {
	mu      sync.Mutex
	entries []compliance.AuditEntry
}

func (w *recordingAuditWriter) Writer(_ context.Context, _ core.Host, e compliance.AuditEntry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = append(w.entries, e)
}

func (w *recordingAuditWriter) WriterSync(ctx context.Context, h core.Host, e compliance.AuditEntry) error {
	w.Writer(ctx, h, e)
	return nil
}

func (w *recordingAuditWriter) actions() map[string]compliance.AuditEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]compliance.AuditEntry{}
	for _, e := range w.entries {
		out[e.Action] = e
	}
	return out
}

func TestDeviceLogin_ApproveThenExchangeOnce(t *testing.T) {
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)
	rg := newTokenRig(t, WithScalingHost(auditOnlyHost{}))
	admin, session := rg.user("admin")
	const device = "203.0.113.20:5000"

	started := rg.startDevice(device)
	assert.Len(t, started.DeviceCode, 43, "32 random bytes, base64url without padding")
	assert.Regexp(t, userCodeShape, started.UserCode)
	assert.Equal(t, "http://localhost:5173/admin/device", started.VerificationURI, "no console URL outside production: the console dev server")
	assert.Equal(t, "http://localhost:5173/admin/device?code="+started.UserCode, started.VerificationURIComplete)
	assert.Equal(t, 600, started.ExpiresIn)
	assert.Equal(t, 5, started.Interval)

	code, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "authorization_pending", body["error"])

	// The page reads the request first, typed however the person typed it.
	typed := strings.ToLower(strings.ReplaceAll(started.UserCode, "-", ""))
	code, body = rg.decide(typed, "", session)
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "cli on build-box", body["client_name"])
	assert.Equal(t, "203.0.113.20", body["requester_ip"])
	assert.Equal(t, "default", body["tenant_id"])
	assert.Equal(t, started.UserCode, body["user_code"])
	assert.Equal(t, []any{"admin"}, body["roles"], "the roles the session would carry")
	assert.EqualValues(t, rg.cfg.JWTExpirySecs, body["session_expires_in"])
	assert.Equal(t, "192.0.2.1", body["approver_ip"], "the approver's own address, beside the requester's")
	assert.Equal(t, false, body["same_address"])

	code, body = rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code, body)
	code, body = rg.decide(started.UserCode, "approve", session)
	assert.Equal(t, http.StatusConflict, code, "one decision per request")
	assert.Equal(t, "decided", body["reason"])
	code, body = rg.decide(started.UserCode, "deny", session)
	assert.Equal(t, http.StatusConflict, code)
	assert.Equal(t, "decided", body["reason"])
	code, body = rg.decide(started.UserCode, "", session)
	assert.Equal(t, http.StatusNotFound, code, "a decided request is no longer shown")
	assert.Equal(t, "decided", body["reason"])

	code, body = rg.pollDevice(started.DeviceCode, device)
	require.Equal(t, http.StatusOK, code, body)
	token, _ := body["token"].(string)
	require.NotEmpty(t, token)
	assert.Equal(t, "Bearer", body["token_type"])
	assert.EqualValues(t, rg.cfg.JWTExpirySecs, body["expires_in"])
	user, _ := body["user"].(map[string]any)
	assert.Equal(t, admin.Email, user["email"])

	claims, err := auth.Parse(rg.cfg.JWTSecret, token)
	require.NoError(t, err)
	assert.Equal(t, "session", claims.TokenType)
	assert.Equal(t, admin.ID.String(), claims.UserID)
	assert.Equal(t, "default", claims.TenantID)
	assert.Equal(t, []string{"admin"}, claims.Roles)
	assert.Equal(t, admin.TokenVersion, claims.TokenVersion)
	assert.False(t, claims.MFAPending)
	assert.Equal(t, time.Duration(rg.cfg.JWTExpirySecs)*time.Second, claims.ExpiresAt.Sub(claims.IssuedAt.Time),
		"the session lasts exactly the configured session expiry")

	// A second exchange of the same device code gets nothing.
	code, body = rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "expired_token", body["error"])

	// It is an ordinary session: a session-only route takes it, and a logout
	// ends it.
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/admin-tokens", token))
	assert.Equal(t, http.StatusOK, rg.get("/api/admin/auth/me", token))
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/logout", bearer: token, body: map[string]any{}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusUnauthorized, rg.get("/api/admin/auth/me", token))

	got := audit.actions()
	for _, action := range []string{"auth.device_login.approve", "auth.device_login.exchange"} {
		e, ok := got[action]
		require.True(t, ok, "no %s audit entry in %v", action, got)
		require.NotNil(t, e.UserID, action)
		assert.Equal(t, "default", e.TenantID, action)
		assert.Contains(t, e.AfterJSON, `"requester_ip":"203.0.113.20"`, action)
	}
	assert.Equal(t, "203.0.113.20", got["auth.device_login.exchange"].IP)
}

func TestDeviceLogin_SuperAdminSessionRunsSchemaImport(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("super_admin")
	const device = "203.0.113.21:5000"
	started := rg.startDevice(device)
	code, body := rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code, body)
	code, body = rg.pollDevice(started.DeviceCode, device)
	require.Equal(t, http.StatusOK, code, body)
	token := body["token"].(string)

	// Schema import is refused to every admin token. The device session is
	// a person's, so it reaches the handler, which answers the empty body.
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/schemas/import", bearer: token, body: map[string]any{}})
	assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

func TestDeviceLogin_Deny(t *testing.T) {
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)
	rg := newTokenRig(t, WithScalingHost(auditOnlyHost{}))
	_, session := rg.user("admin")
	const device = "203.0.113.22:5000"
	started := rg.startDevice(device)

	code, body := rg.decide(started.UserCode, "deny", session)
	require.Equal(t, http.StatusOK, code, body)
	code, body = rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "access_denied", body["error"])
	code, _ = rg.decide(started.UserCode, "approve", session)
	assert.Equal(t, http.StatusConflict, code, "a denied request cannot be approved after")

	e, ok := audit.actions()["auth.device_login.deny"]
	require.True(t, ok)
	assert.Contains(t, e.AfterJSON, `"requester_ip":"203.0.113.22"`)
}

func TestDeviceLogin_SlowDownGrowsTheInterval(t *testing.T) {
	rg := newTokenRig(t)
	const device = "203.0.113.23:5000"
	started := rg.startDevice(device)

	code, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "authorization_pending", body["error"])
	code, body = rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "slow_down", body["error"])
	assert.EqualValues(t, 10, body["interval"])
	_, body = rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, "slow_down", body["error"])
	assert.EqualValues(t, 15, body["interval"])

	// After waiting the grown interval the device is answered normally.
	store := db.NewDeviceLoginStore(rg.pool)
	_, err := rg.pool.Exec(context.Background(), `UPDATE sys_device_logins SET last_polled_at = $1 WHERE user_code = $2`,
		time.Now().Add(-16*time.Second).UTC(), strings.ReplaceAll(started.UserCode, "-", ""))
	require.NoError(t, err)
	_, body = rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, "authorization_pending", body["error"])
	d, err := store.GetByUserCode(context.Background(), strings.ReplaceAll(started.UserCode, "-", ""))
	require.NoError(t, err)
	assert.Equal(t, 15, d.PollInterval)
}

func TestDeviceLogin_Expiry(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	const device = "203.0.113.24:5000"
	started := rg.startDevice(device)
	stored := strings.ReplaceAll(started.UserCode, "-", "")
	_, err := rg.pool.Exec(context.Background(), `UPDATE sys_device_logins SET expires_at = $1 WHERE user_code = $2`,
		time.Now().Add(-time.Minute).UTC(), stored)
	require.NoError(t, err)

	code, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "expired_token", body["error"])
	code, body = rg.decide(started.UserCode, "", session)
	assert.Equal(t, http.StatusNotFound, code)
	assert.Equal(t, "expired", body["reason"])
	code, _ = rg.decide(started.UserCode, "approve", session)
	assert.Equal(t, http.StatusNotFound, code)

	// An approval that expires before the device polls is not exchanged.
	late := rg.startDevice("203.0.113.25:5000")
	code, _ = rg.decide(late.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code)
	_, err = rg.pool.Exec(context.Background(), `UPDATE sys_device_logins SET expires_at = $1 WHERE user_code = $2`,
		time.Now().Add(-time.Minute).UTC(), strings.ReplaceAll(late.UserCode, "-", ""))
	require.NoError(t, err)
	_, body = rg.pollDevice(late.DeviceCode, "203.0.113.25:5000")
	assert.Equal(t, "expired_token", body["error"])
}

func TestDeviceLogin_OnlyAnAdminSessionDecides(t *testing.T) {
	keyClaims := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") == "" {
				next.ServeHTTP(w, r)
				return
			}
			claims := &core.AuthClaims{UserID: uuid.NewString(), Roles: []string{"super_admin"}, TenantID: "default", IsAPIKey: true}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), core.ClaimsKey, claims)))
		})
	}
	rg := newTokenRig(t, WithAPIKeyAuth(keyClaims))
	_, editor := rg.user("editor")
	_, admin := rg.user("admin")
	adminToken, _ := rg.issue(admin, []string{core.AdminGrantSchemasRead}, nil)
	started := rg.startDevice("203.0.113.26:5000")

	for _, action := range []string{"", "approve", "deny"} {
		code, _ := rg.decide(started.UserCode, action, editor)
		assert.Equal(t, http.StatusForbidden, code, "editor %q", action)
		code, _ = rg.decide(started.UserCode, action, adminToken)
		assert.Equal(t, http.StatusForbidden, code, "admin token %q", action)

		method, path := http.MethodPost, "/api/admin/auth/device/"+started.UserCode+"/"+action
		if action == "" {
			method, path = http.MethodGet, "/api/admin/auth/device/"+started.UserCode
		}
		rec := rg.do(tokenCall{method: method, path: path, body: map[string]any{}, header: map[string]string{"X-API-Key": "k"}})
		// An API key with an admin role is refused on the whole admin API.
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "api key %q", action)
		rec = rg.do(tokenCall{method: method, path: path, body: map[string]any{}})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "no credential %q", action)
	}

	// Still pending after every refused attempt.
	code, body := rg.pollDevice(started.DeviceCode, "203.0.113.26:5000")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "authorization_pending", body["error"])
}

func TestDeviceLogin_UnknownCodes(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")

	for _, c := range []string{"WDJB-MJHT", "0000-0000", "short", "ABCD-EFGH-JKMN"} {
		code, body := rg.decide(c, "", session)
		assert.Equal(t, http.StatusNotFound, code, c)
		assert.Equal(t, "unknown", body["reason"], c)
		code, _ = rg.decide(c, "approve", session)
		assert.Equal(t, http.StatusNotFound, code, c)
	}

	code, body := rg.pollDevice("not-a-device-code", "203.0.113.27:5000")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "invalid_grant", body["error"])
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/token", body: map[string]any{}, remote: "203.0.113.27:5000"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid_request")
}

func TestDeviceLogin_ClientNameIsChecked(t *testing.T) {
	rg := newTokenRig(t)
	for i, name := range []string{"", "   ", strings.Repeat("x", 65), "bad\x07name"} {
		rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": name}, remote: "203.0.113.28:" + string(rune('0'+i)) + "000"})
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%q: %s", name, rec.Body.String())
	}
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": strings.Repeat("é", 64)}, remote: "203.0.113.29:5000"})
	assert.Equal(t, http.StatusOK, rec.Code, "64 characters is the limit, not 64 bytes")
}

func TestDeviceLogin_StartIsRateLimited(t *testing.T) {
	rg := newTokenRig(t)
	limited := false
	for i := 0; i < 8; i++ {
		rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli"}, remote: "203.0.113.30:5000"})
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.True(t, limited, "eight starts from one address in a row were all accepted")
	// Another address is unaffected.
	rg.startDevice("203.0.113.31:5000")
}

func TestDeviceLogin_OpenRequestsPerAddressAreCapped(t *testing.T) {
	rg := newTokenRig(t)
	store := db.NewDeviceLoginStore(rg.pool)
	now := time.Now().UTC()
	for i := 0; i < deviceLoginPendingPerAddress; i++ {
		_, userCode, err := newDeviceCodes()
		require.NoError(t, err)
		require.NoError(t, store.Create(context.Background(), &domain.DeviceLogin{
			ID: uuid.New(), DeviceCodeHash: hashDeviceCode(uuid.NewString()), UserCode: userCode,
			ClientName: "seeded", RequesterIP: "203.0.113.32", RequesterNet: "203.0.113.32", PollInterval: 5, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		}))
	}
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli"}, remote: "203.0.113.32:5000"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "waiting for approval")
}

func TestDeviceLogin_EndedSessionsVoidAnApproval(t *testing.T) {
	rg := newTokenRig(t)
	admin, session := rg.user("admin")
	const device = "203.0.113.33:5000"
	started := rg.startDevice(device)
	code, _ := rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code)

	// A logout or password change after approving ends every session the
	// account holds, the one waiting to be exchanged included.
	require.NoError(t, rg.users.BumpTokenVersion(context.Background(), admin.ID))
	code, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "access_denied", body["error"])
}

func TestDeviceLogin_DemotedApproverGetsNoSession(t *testing.T) {
	rg := newTokenRig(t)
	admin, session := rg.user("admin")
	const device = "203.0.113.34:5000"
	started := rg.startDevice(device)
	code, _ := rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code)

	_, err := rg.users.UpdateRoles(context.Background(), admin.ID, []string{"editor"})
	require.NoError(t, err)
	_, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, "access_denied", body["error"])
}

func TestDeviceLogin_ExpiredAccountGetsNoSession(t *testing.T) {
	rg := newTokenRig(t)
	admin, session := rg.user("admin")
	const device = "203.0.113.35:5000"
	started := rg.startDevice(device)
	code, _ := rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code)

	past := time.Now().Add(-time.Minute)
	pastPtr := &past
	_, err := rg.users.SetAccountState(context.Background(), admin.ID, nil, &pastPtr)
	require.NoError(t, err)
	_, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, "access_denied", body["error"])
}

// The page a person approves on is the console's, so the verification URI is
// built on the console URL and never on the engine's base URL, which on a split
// topology names a host that serves no such page.
func TestDeviceLogin_VerificationURIIsOnTheConsole(t *testing.T) {
	cases := []struct {
		name       string
		consoleURL string
		production bool
		want       string
		wantErr    error
	}{
		{"console URL", "https://admin.example.com", true, "https://admin.example.com/admin/device", nil},
		{"trailing slash", "https://admin.example.com/", false, "https://admin.example.com/admin/device", nil},
		{"console under a path", "https://example.com/console", true, "https://example.com/console/admin/device", nil},
		{"unset outside production", "", false, core.DevConsoleURL + "/admin/device", nil},
		{"unset in production", "", true, "", core.ErrConsoleURLUnset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &deviceLoginHandler{consoleURL: tc.consoleURL, production: tc.production}
			got, err := h.verificationURI()
			assert.ErrorIs(t, err, tc.wantErr)
			assert.Equal(t, tc.want, got)
		})
	}
}

// A request on a split topology is answered with the console's page, whatever
// LYEVE_BASE_URL says.
func TestDeviceLogin_StartAnswersTheConsolePage(t *testing.T) {
	rg := newConfiguredTokenRig(t, func(cfg *config.Config) {
		cfg.BaseURL = "https://api.example.com"
		cfg.ConsoleURL = "https://admin.example.com"
	}, WithScalingHost(auditOnlyHost{}))
	started := rg.startDevice("203.0.113.21:5000")
	assert.Equal(t, "https://admin.example.com/admin/device", started.VerificationURI)
	assert.Equal(t, "https://admin.example.com/admin/device?code="+started.UserCode, started.VerificationURIComplete)
}

// In production with no console URL there is nowhere to send the person, so
// the request is refused before a code is issued rather than answered with a
// page on a guessed host.
func TestDeviceLogin_StartRefusesWithoutAConsoleURLInProduction(t *testing.T) {
	rg := newConfiguredTokenRig(t, func(cfg *config.Config) {
		cfg.Environment = "production"
		cfg.BaseURL = "https://api.example.com"
	}, WithScalingHost(auditOnlyHost{}))
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli on build-box"}, remote: "203.0.113.22:5000"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "device_code")

	var open int
	row, err := rg.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sys_device_logins`)
	require.NoError(t, err)
	require.NoError(t, row.Scan(&open))
	assert.Zero(t, open, "a refused request left a row behind")
}

func TestNormalizeUserCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"WDJB-MJHT", "WDJBMJHT", true},
		{"wdjb-mjht", "WDJBMJHT", true},
		{"wdjbmjht", "WDJBMJHT", true},
		{" WDJB MJHT ", "WDJBMJHT", true},
		{"WDJB-MJH0", "", false},
		{"WDJB-MJHO", "", false},
		{"WDJB-MJH1", "", false},
		{"WDJB-MJHI", "", false},
		{"WDJB-MJHL", "", false},
		{"WDJB-MJ", "", false},
		{"WDJB-MJHTX", "", false},
		{"", "", false},
		{"WDJB%2FMJHT", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeUserCode(c.in)
		assert.Equal(t, c.ok, ok, c.in)
		assert.Equal(t, c.want, got, c.in)
	}
}

func TestNewDeviceCodes_UseTheUnambiguousAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		device, user, err := newDeviceCodes()
		require.NoError(t, err)
		assert.Len(t, device, 43)
		assert.Len(t, user, userCodeLength)
		assert.Regexp(t, userCodeShape, displayUserCode(user))
		assert.False(t, seen[device], "a device code repeated")
		seen[device] = true
	}
}

func TestDeviceLogin_ApprovalNeedsTheAccountPassword(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	const device = "203.0.113.40:5000"
	started := rg.startDevice(device)

	rec := rg.approveWith(started.UserCode, session, map[string]any{})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "password is required")
	rec = rg.approveWith(started.UserCode, session, map[string]any{"password": "wrong-password"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "The password is not valid.")

	code, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "authorization_pending", body["error"], "a refused step-up approves nothing")
}

func TestDeviceLogin_WrongPasswordsLockTheAccount(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	started := rg.startDevice("203.0.113.41:5000")
	// The decision routes are limited to a burst of ten per address, so each
	// attempt comes from its own address: the lockout is what is under test.
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/" + started.UserCode + "/approve",
			bearer: session, body: map[string]any{"password": "wrong-password"}, remote: "198.51.100." + string(rune('1'+i)) + ":4000"})
	}
	assert.Equal(t, http.StatusTooManyRequests, last.Code, last.Body.String())
	// Locked, the right password is refused too.
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/" + started.UserCode + "/approve",
		bearer: session, body: map[string]any{"password": tokenTestPassword}, remote: "198.51.100.90:4000"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestDeviceLogin_CookieApprovalNeedsTheCSRFToken(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	started := rg.startDevice("203.0.113.42:5000")
	path := "/api/admin/auth/device/" + started.UserCode + "/approve"
	body := map[string]any{"password": tokenTestPassword}

	rec := rg.do(tokenCall{method: http.MethodPost, path: path, body: body, header: map[string]string{"Cookie": security.SessionCookieNameInsecure + "=" + session}})
	assert.Equal(t, http.StatusForbidden, rec.Code, "a cookie without the CSRF pair: %s", rec.Body.String())

	rec = rg.do(tokenCall{method: http.MethodPost, path: path, body: body, header: map[string]string{
		"Cookie":       security.SessionCookieNameInsecure + "=" + session + "; " + csrfCookieName + "=pair",
		"X-CSRF-Token": "pair",
	}})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestDeviceLogin_ChallengeTokenCannotApprove(t *testing.T) {
	rg := newTokenRig(t)
	u, _ := rg.user("admin")
	challenge, err := auth.SignChallenge(rg.cfg.JWTSecret, u.ID, u.Email, u.Roles, "default")
	require.NoError(t, err)
	started := rg.startDevice("203.0.113.43:5000")
	for _, action := range []string{"", "approve", "deny"} {
		code, _ := rg.decide(started.UserCode, action, challenge)
		assert.Equal(t, http.StatusUnauthorized, code, "an MFA challenge is not a session: %q", action)
	}
}

func TestDeviceLogin_DisabledAccountGetsNoSession(t *testing.T) {
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)
	rg := newTokenRig(t, WithScalingHost(auditOnlyHost{}))
	admin, session := rg.user("admin")
	const device = "203.0.113.44:5000"
	started := rg.startDevice(device)
	code, _ := rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code)

	off := true
	_, err := rg.users.SetAccountState(context.Background(), admin.ID, &off, nil)
	require.NoError(t, err)
	_, body := rg.pollDevice(started.DeviceCode, device)
	assert.Equal(t, "access_denied", body["error"])

	e, ok := audit.actions()["auth.device_login.refused"]
	require.True(t, ok, "a refused exchange is audited")
	assert.Contains(t, e.AfterJSON, `"reason":"account_disabled"`)
	require.NotNil(t, e.UserID)
}

func TestDeviceLogin_ConcurrentExchangesYieldOneSession(t *testing.T) {
	rg := newTokenRig(t)
	_, session := rg.user("admin")
	started := rg.startDevice("203.0.113.45:5000")
	code, _ := rg.decide(started.UserCode, "approve", session)
	require.Equal(t, http.StatusOK, code)

	const polls = 8
	var wg sync.WaitGroup
	statuses := make([]int, polls)
	for i := 0; i < polls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/token",
				body: map[string]any{"device_code": started.DeviceCode}, remote: "203.0.113.45:5000"})
			statuses[i] = rec.Code
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		if s == http.StatusOK {
			ok++
		}
	}
	assert.Equal(t, 1, ok, "statuses %v", statuses)
}

func TestDeviceLogin_SuperAdminBindsTheTenantItActsIn(t *testing.T) {
	rg := newTokenRig(t, WithTenantValidator(func(_ context.Context, slug string) bool { return slug == "globex" }))
	_, session := rg.user("super_admin")
	const device = "203.0.113.46:5000"
	started := rg.startDevice(device)

	rec := rg.do(tokenCall{method: http.MethodGet, path: "/api/admin/auth/device/" + started.UserCode, bearer: session,
		header: map[string]string{"X-Tenant-ID": "globex"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"tenant_id":"globex"`)
	rec = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/" + started.UserCode + "/approve", bearer: session,
		header: map[string]string{"X-Tenant-ID": "globex"}, body: map[string]any{"password": tokenTestPassword}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	code, body := rg.pollDevice(started.DeviceCode, device)
	require.Equal(t, http.StatusOK, code, body)
	claims, err := auth.Parse(rg.cfg.JWTSecret, body["token"].(string))
	require.NoError(t, err)
	assert.Equal(t, "globex", claims.TenantID)
	assert.Equal(t, []string{"super_admin"}, claims.Roles)
}

func TestDeviceLogin_OpenRequestsAreCappedPerIPv6Prefix(t *testing.T) {
	rg := newTokenRig(t)
	store := db.NewDeviceLoginStore(rg.pool)
	now := time.Now().UTC()
	for i := 0; i < deviceLoginPendingPerAddress; i++ {
		_, userCode, err := newDeviceCodes()
		require.NoError(t, err)
		ip := fmt.Sprintf("2001:db8:1:2::%x", i+1)
		require.NoError(t, store.Create(context.Background(), &domain.DeviceLogin{
			ID: uuid.New(), DeviceCodeHash: hashDeviceCode(uuid.NewString()), UserCode: userCode,
			ClientName: "seeded", RequesterIP: ip, RequesterNet: requesterNet(ip), PollInterval: 5, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		}))
	}
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli"}, remote: "[2001:db8:1:2::ffff]:5000"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "another address in the same /64: %s", rec.Body.String())
	rec = rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli"}, remote: "[2001:db8:1:3::1]:5000"})
	assert.Equal(t, http.StatusOK, rec.Code, "the next /64 is unaffected: %s", rec.Body.String())
}

func TestRequesterNet(t *testing.T) {
	assert.Equal(t, "203.0.113.7", requesterNet("203.0.113.7"))
	assert.Equal(t, "203.0.113.7", requesterNet("::ffff:203.0.113.7"))
	assert.Equal(t, "2001:db8:1:2::/64", requesterNet("2001:db8:1:2:aaaa:bbbb:cccc:dddd"))
	assert.Equal(t, "not-an-ip", requesterNet("not-an-ip"))
}

func TestDeviceLogin_SingleTenantEmptyHomeBindsNoTenant(t *testing.T) {
	h := &deviceLoginHandler{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(core.WithTenantID(r.Context(), "default"))
	assert.Equal(t, "", h.boundTenant(r, &auth.Claims{}), "the implicit tenant was nobody's choice")
	assert.Equal(t, "default", h.boundTenant(r, &auth.Claims{TenantID: "default"}))
	h.multiTenant = true
	assert.Equal(t, "default", h.boundTenant(r, &auth.Claims{}), "on a multi-tenant install the tenant was named")
}
