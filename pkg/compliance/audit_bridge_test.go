package compliance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureAuditWriter is an in-memory AuditLogWriter that records the
// last AuditEntry so tests can assert what the kernel assembled and routed.
type captureAuditWriter struct {
	asyncCalls int
	syncCalls  int
	syncErr    error
	last       AuditEntry
}

func (w *captureAuditWriter) Writer(_ context.Context, _ core.Host, entry AuditEntry) {
	w.asyncCalls++
	w.last = entry
}

func (w *captureAuditWriter) WriterSync(_ context.Context, _ core.Host, entry AuditEntry) error {
	w.syncCalls++
	w.last = entry
	return w.syncErr
}

func TestRecordAudit_NilHostIsNoOp(t *testing.T) {
	ClearAuditWriter()
	// Must not panic and must not touch any writer.
	assert.NotPanics(t, func() {
		RecordAudit(context.Background(), nil, "login", "user", "u1", "1.2.3.4", "curl")
	})
}

func TestRecordAudit_AsyncRoutesToWriter(t *testing.T) {
	w := &captureAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	RecordAudit(context.Background(), nil, "login", "user", "u1", "1.2.3.4", "curl/8")

	assert.Equal(t, 1, w.asyncCalls)
	assert.Equal(t, 0, w.syncCalls)
	assert.Equal(t, "login", w.last.Action)
	assert.Equal(t, "user", w.last.ResourceType)
	assert.Equal(t, "1.2.3.4", w.last.IP)
	assert.Equal(t, "curl/8", w.last.UserAgent)
}

func TestRecordAuditWithState_SyncCapturesClaimsAndState(t *testing.T) {
	w := &captureAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	ctx := context.WithValue(context.Background(), core.ClaimsKey,
		&core.AuthClaims{UserID: uuid.New().String(), TenantID: "acme"})

	before := map[string]any{"status": "draft"}
	after := map[string]any{"status": "published"}
	RecordAuditWithState(ctx, nil, "update", "doc", "d1", "ip", "ua", before, after, WithSync())

	assert.Equal(t, 1, w.syncCalls)
	assert.Equal(t, 0, w.asyncCalls)
	assert.Equal(t, "acme", w.last.TenantID, "tenant taken from claims")
	assert.NotNil(t, w.last.UserID, "valid UUID claim resolves to a userID")
	assert.JSONEq(t, `{"status":"draft"}`, w.last.BeforeJSON)
	assert.JSONEq(t, `{"status":"published"}`, w.last.AfterJSON)
}

func TestRecordAuditWithState_TenantFallbackFromContext(t *testing.T) {
	w := &captureAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	// Claims carry no tenant, but the tenant context key does.
	ctx := core.WithTenantID(
		context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{UserID: uuid.New().String()}),
		"ctx-tenant")

	RecordAudit(ctx, nil, "read", "doc", "d1", "ip", "ua")
	assert.Equal(t, "ctx-tenant", w.last.TenantID)
}

func TestRecordAuditWithState_ActingTenantWinsOverClaim(t *testing.T) {
	w := &captureAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	// A super_admin whose home tenant is "default" acting on "acme" through an
	// X-Tenant-ID override. The row must land in the tenant that was acted on,
	// otherwise acme's own log never shows the action.
	ctx := core.WithTenantID(
		context.WithValue(context.Background(), core.ClaimsKey,
			&core.AuthClaims{UserID: uuid.New().String(), TenantID: "default"}),
		"acme")

	RecordAudit(ctx, nil, "apikey.create", "api_key", "k1", "ip", "ua")
	assert.Equal(t, "acme", w.last.TenantID)
}

func TestRecordAuditWithState_SyncErrorIsLoggedNotPanicked(t *testing.T) {
	w := &captureAuditWriter{syncErr: errors.New("write failed")}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	// A failing sync write is logged via host.Logger and swallowed.
	assert.NotPanics(t, func() {
		RecordAuditWithState(context.Background(), nil, "delete", "doc", "d1", "ip", "ua", nil, nil, WithSync())
	})
	assert.Equal(t, 1, w.syncCalls)
}

func TestRecordAuditFromRequest(t *testing.T) {
	w := &captureAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/content", nil)
	req.RemoteAddr = "10.0.0.5:4444"
	req.Header.Set("User-Agent", "test-agent")

	RecordAuditFromRequest(req, nil, "create", "content", "c1")
	require.Equal(t, 1, w.asyncCalls)
	assert.Equal(t, "10.0.0.5:4444", w.last.IP)
	assert.Equal(t, "test-agent", w.last.UserAgent)
	assert.Equal(t, "create", w.last.Action)

	RecordAuditFromRequestWithState(req, nil, "update", "content", "c1",
		map[string]any{"v": 1}, map[string]any{"v": 2})
	assert.Equal(t, 2, w.asyncCalls)
	assert.JSONEq(t, `{"v":1}`, w.last.BeforeJSON)
	assert.JSONEq(t, `{"v":2}`, w.last.AfterJSON)
}

// A change an admin token makes is recorded as its owner's, with the token
// named beside them, and without the handler that records it knowing.
func TestRecordAudit_AdminTokenIsNamedAsTheActor(t *testing.T) {
	w := &captureAuditWriter{}
	RegisterAuditWriter(w)
	t.Cleanup(ClearAuditWriter)

	owner := uuid.New()
	tokenID := uuid.NewString()
	ctx := context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{
		UserID: owner.String(), TenantID: "acme", AdminTokenID: tokenID,
	})
	RecordAudit(ctx, nil, "webhook.create", "webhook", "w1", "203.0.113.9", "deploy-bot/2")

	assert.Equal(t, tokenID, w.last.AdminTokenID)
	assert.Equal(t, "admin-token:"+tokenID+" deploy-bot/2", w.last.UserAgent)
	if uid, ok := w.last.UserID.(*uuid.UUID); assert.True(t, ok) {
		assert.Equal(t, owner, *uid)
	}

	// A session caller's row is unchanged.
	ctx = context.WithValue(context.Background(), core.ClaimsKey, &core.AuthClaims{UserID: owner.String()})
	RecordAudit(ctx, nil, "webhook.create", "webhook", "w1", "203.0.113.9", "browser")
	assert.Empty(t, w.last.AdminTokenID)
	assert.Equal(t, "browser", w.last.UserAgent)
}

func TestActorUserAgent(t *testing.T) {
	assert.Equal(t, "curl/8", ActorUserAgent("", "curl/8"))
	assert.Equal(t, "admin-token:t1", ActorUserAgent("t1", ""))
	assert.Equal(t, "admin-token:t1 curl/8", ActorUserAgent("t1", "curl/8"))

	// A caller cannot pass itself off as a token by its user agent.
	assert.Equal(t, "curl/8", ActorUserAgent("", "admin-token:forged curl/8"))
	assert.Equal(t, "", ActorUserAgent("", "admin-token:forged"))
	assert.Equal(t, "curl/8", ActorUserAgent("", "admin-token:a admin-token:b curl/8"))
	assert.Equal(t, "admin-token:t1 curl/8", ActorUserAgent("t1", "admin-token:forged curl/8"))
}
