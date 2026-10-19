package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// hooliHost is a custom domain the install maps onto the tenant hooli.
const hooliHost = "sign-in.hooli.example"

// mapHooliDomain stands in for a plugin that resolves a tenant from a custom
// domain in the Host header, ahead of TenantHeader.
func mapHooliDomain(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == hooliHost {
			r = r.WithContext(core.WithTenantID(r.Context(), "hooli"))
		}
		next.ServeHTTP(w, r)
	})
}

// loggedLine is one record as the engine's enrich handler passes it on, with
// every attribute it carries.
type loggedLine struct {
	message string
	attrs   map[string]string
}

type lineRecorder struct {
	mu    *sync.Mutex
	lines *[]loggedLine
}

func (l lineRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l lineRecorder) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l lineRecorder) WithGroup(string) slog.Handler            { return l }

func (l lineRecorder) Handle(_ context.Context, r slog.Record) error {
	line := loggedLine{message: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	*l.lines = append(*l.lines, line)
	l.mu.Unlock()
	return nil
}

// captureLines routes the default logger through the enrich handler at Debug
// and returns a reader that drains what was logged since the last read.
func captureLines(t *testing.T) func() []loggedLine {
	t.Helper()
	var mu sync.Mutex
	var lines []loggedLine
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewEnrichHandler(lineRecorder{mu: &mu, lines: &lines}, logging.NewLevelMap(slog.LevelDebug))))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []loggedLine {
		mu.Lock()
		defer mu.Unlock()
		out := lines
		lines = nil
		return out
	}
}

// requireLine returns the one line whose message starts with prefix.
func requireLine(t *testing.T, lines []loggedLine, prefix string) loggedLine {
	t.Helper()
	var found []loggedLine
	for _, l := range lines {
		if strings.HasPrefix(l.message, prefix) {
			found = append(found, l)
		}
	}
	require.Len(t, found, 1, "lines starting %q in %v", prefix, lines)
	return found[0]
}

// assertNoLineNames fails on any line that carries tenant as its stamp or in
// any attribute. The access line is skipped: a request that arrived on a
// tenant's domain is that tenant's own, so its line keeps the stamp.
func assertNoLineNames(t *testing.T, lines []loggedLine, tenant, scenario string) {
	t.Helper()
	for _, l := range lines {
		if l.message == "request" {
			continue
		}
		for k, v := range l.attrs {
			assert.NotEqual(t, tenant, v, "%s: %q carries %s=%s", scenario, l.message, k, v)
		}
	}
}

func (rg *tokenRig) startOnHooli() int {
	rg.t.Helper()
	rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device", body: map[string]any{"client_name": "cli on build-box"},
		remote: "203.0.113.60:5000", header: map[string]string{"Host": hooliHost}})
	return rec.Code
}

// An anonymous sign-in started on hooli's domain writes lines about the
// install's own device sign-in table: the console URL is unset, the prune
// failed, the install holds its limit of open requests, a row could not be
// written. None is hooli's to read, so each carries no tenant.
func TestDeviceLogin_StartLinesAboutTheInstallCarryNoTenant(t *testing.T) {
	lines := captureLines(t)
	valid := WithTenantValidator(func(_ context.Context, slug string) bool { return slug == "hooli" })

	unset := newConfiguredTokenRig(t, func(cfg *config.Config) {
		cfg.Environment = "production"
		cfg.BaseURL = "https://api.example.com"
	}, WithPreAuthMiddleware(mapHooliDomain), valid)
	lines()
	assert.Equal(t, http.StatusServiceUnavailable, unset.startOnHooli())
	got := lines()
	line := requireLine(t, got, "device login refused: the console URL is unset")
	assert.NotContains(t, line.attrs, "tenant_id")
	assertNoLineNames(t, got, "hooli", "console URL unset")

	rg := newTokenRig(t, WithPreAuthMiddleware(mapHooliDomain), valid)
	ctx := context.Background()

	_, err := rg.pool.Exec(ctx, `INSERT INTO sys_device_logins (id, device_code_hash, user_code, client_name, requester_ip,
		requester_net, status, poll_interval, created_at, expires_at)
		SELECT gen_random_uuid(), md5(g::text), 'SEED' || g, 'seeded', '198.51.100.9', '198.51.100.9', 'pending', 5,
			NOW(), NOW() + INTERVAL '10 minutes'
		FROM generate_series(1, $1) g`, deviceLoginPendingMax)
	require.NoError(t, err)
	lines()
	assert.Equal(t, http.StatusServiceUnavailable, rg.startOnHooli())
	got = lines()
	line = requireLine(t, got, "device login refused: the install holds its limit of open requests")
	assert.NotContains(t, line.attrs, "tenant_id")
	assertNoLineNames(t, got, "hooli", "install at its limit")
	_, err = rg.pool.Exec(ctx, `DELETE FROM sys_device_logins`)
	require.NoError(t, err)

	_, err = rg.pool.Exec(ctx, `CREATE FUNCTION refuse_device_logins() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'device sign-in table refused'; END $$`)
	require.NoError(t, err)
	_, err = rg.pool.Exec(ctx, `CREATE TRIGGER refuse_prune BEFORE DELETE ON sys_device_logins
		FOR EACH STATEMENT EXECUTE FUNCTION refuse_device_logins()`)
	require.NoError(t, err)
	lines()
	assert.Equal(t, http.StatusOK, rg.startOnHooli(), "a failed prune does not stop the sign-in")
	got = lines()
	line = requireLine(t, got, "device login prune failed")
	assert.NotContains(t, line.attrs, "tenant_id")
	assertNoLineNames(t, got, "hooli", "prune failed")

	_, err = rg.pool.Exec(ctx, `CREATE TRIGGER refuse_create BEFORE INSERT ON sys_device_logins
		FOR EACH STATEMENT EXECUTE FUNCTION refuse_device_logins()`)
	require.NoError(t, err)
	lines()
	assert.Equal(t, http.StatusServiceUnavailable, rg.startOnHooli())
	got = lines()
	line = requireLine(t, got, "device login create failed")
	assert.NotContains(t, line.attrs, "tenant_id")
	assertNoLineNames(t, got, "hooli", "create failed")
}

// failingRiskAssessor keeps the tenant of every login it is asked to record
// and refuses each one. The device history stores a login under the tenant
// on its context.
type failingRiskAssessor struct {
	security.DeviceRiskAssessor
	mu      sync.Mutex
	tenants []string
}

func (a *failingRiskAssessor) RecordLogin(ctx context.Context, _ uuid.UUID, _ security.DeviceFingerprint, _ bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tenants = append(a.tenants, core.TenantIDFromCtx(ctx))
	return errors.New("device history unavailable")
}

// A super admin approves a sign-in for globex, and the device polls on
// hooli's domain. The exchange, its failed device history record and a
// refused exchange are about the globex session: hooli reads none of them,
// and globex keeps what is its own.
func TestDeviceLogin_ExchangeLinesCarryTheSessionsTenant(t *testing.T) {
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)
	lines := captureLines(t)
	devices := &failingRiskAssessor{}
	rg := newTokenRig(t, WithScalingHost(auditOnlyHost{}), WithPreAuthMiddleware(mapHooliDomain), WithDeviceRiskAssessor(devices),
		WithTenantValidator(func(_ context.Context, slug string) bool { return slug == "globex" || slug == "hooli" }))
	admin, session := rg.user("super_admin")
	const device = "203.0.113.61:5000"
	poll := func(deviceCode string) (int, string) {
		rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/token", body: map[string]any{"device_code": deviceCode},
			remote: device, header: map[string]string{"Host": hooliHost}})
		return rec.Code, rec.Body.String()
	}
	approve := func() string {
		started := rg.startDevice(device)
		rec := rg.do(tokenCall{method: http.MethodPost, path: "/api/admin/auth/device/" + started.UserCode + "/approve", bearer: session,
			header: map[string]string{"X-Tenant-ID": "globex"}, body: map[string]any{"password": tokenTestPassword}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return started.DeviceCode
	}

	deviceCode := approve()
	lines()
	code, body := poll(deviceCode)
	require.Equal(t, http.StatusOK, code, body)
	got := lines()
	line := requireLine(t, got, "failed to record device login")
	assert.Equal(t, "globex", line.attrs["tenant_id"], "the session's tenant reads its own failed record")
	assertNoLineNames(t, got, "hooli", "exchange")
	devices.mu.Lock()
	assert.Equal(t, []string{"globex"}, devices.tenants, "the device history row is the session tenant's")
	devices.mu.Unlock()

	deviceCode = approve()
	require.NoError(t, rg.users.BumpTokenVersion(context.Background(), admin.ID))
	lines()
	code, body = poll(deviceCode)
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, "access_denied")
	got = lines()
	line = requireLine(t, got, "device login exchange refused")
	assert.Equal(t, "globex", line.attrs["tenant_id"], "the bound tenant reads its own refusal")
	assertNoLineNames(t, got, "hooli", "refused exchange")

	entries := audit.actions()
	require.Contains(t, entries, "auth.device_login.exchange")
	require.Contains(t, entries, "auth.device_login.refused")
	audit.mu.Lock()
	defer audit.mu.Unlock()
	for _, e := range audit.entries {
		assert.NotEqual(t, "hooli", e.TenantID, "audit %s", e.Action)
		assert.NotContains(t, e.AfterJSON, "hooli", "audit %s", e.Action)
	}
	assert.Equal(t, "globex", entries["auth.device_login.refused"].TenantID)
	assert.Equal(t, "globex", entries["auth.device_login.exchange"].TenantID)
}
