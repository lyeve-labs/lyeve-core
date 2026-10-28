//go:build !mutest

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Every password sign-in leaves an entry, whether it opened a session, asked
// for a second factor or was refused. The entry names the account and the
// address the attempt came from, and never the password or the code. An
// address that matches no account is named by its digest.
func TestAuthHandler_SignInsAreAudited(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)

	pool := testdb.Postgres(t)
	const secret = "test-secret-at-least-32-bytes-long!!"
	const password = "the-audited-password"
	plain := seedUser(t, pool, "audited-plain@example.com", password, []string{"editor"})
	withMFA := seedUser(t, pool, "audited-mfa@example.com", password, []string{"editor"})
	totpSecret := seedMFA(t, pool, withMFA.ID, secret)

	h := NewAuthHandler(db.NewUserStore(pool), &simpleMFAStore{pool: pool, secret: secret}, pool, secret, 3600, false)
	h.host = auditOnlyHost{}
	// The second-factor account also belongs to another tenant and signs in
	// there, so its entries have to follow the acting tenant, not its home.
	const acting = "audit_acting"
	members := newFakeMemberships()
	_, err := members.Grant(context.Background(), withMFA.ID, acting, []string{"editor"})
	require.NoError(t, err)
	h.WithMemberships(members)

	post := func(handle http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "203.0.113.9:5100"
		rr := httptest.NewRecorder()
		handle(rr, r)
		return rr
	}
	login := func(email, pw string) *httptest.ResponseRecorder {
		return post(h.Login, "/api/admin/auth/login", map[string]string{"email": email, "password": pw})
	}
	last := func(t *testing.T) compliance.AuditEntry {
		t.Helper()
		audit.mu.Lock()
		defer audit.mu.Unlock()
		require.NotEmpty(t, audit.entries, "no audit entry")
		return audit.entries[len(audit.entries)-1]
	}
	assertEntry := func(t *testing.T, e compliance.AuditEntry, action string, account uuid.UUID, actor bool) {
		t.Helper()
		assert.Equal(t, action, e.Action)
		assert.Equal(t, "user", e.ResourceType)
		assert.Equal(t, account.String(), e.ResourceID)
		assert.Equal(t, "203.0.113.9", e.IP)
		if actor {
			require.IsType(t, &uuid.UUID{}, e.UserID)
			assert.Equal(t, account, *e.UserID.(*uuid.UUID))
		} else {
			assert.Nil(t, e.UserID, "a refused attempt has no signed-in actor")
		}
		assert.NotContains(t, e.AfterJSON, password)
	}

	t.Run("password session", func(t *testing.T) {
		require.Equal(t, http.StatusOK, login(plain.Email, password).Code)
		e := last(t)
		assertEntry(t, e, "auth.login", plain.ID, true)
		assert.Contains(t, e.AfterJSON, `"method":"password"`)
	})

	t.Run("wrong password", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, login(plain.Email, "not-the-password").Code)
		e := last(t)
		assertEntry(t, e, "auth.login.refused", plain.ID, false)
		assert.Contains(t, e.AfterJSON, `"status":401`)
	})

	t.Run("unknown address", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, login("nobody-here@example.com", password).Code)
		e := last(t)
		assert.Equal(t, "auth.login.refused", e.Action)
		assert.Empty(t, e.ResourceID)
		assert.Contains(t, e.AfterJSON, `"subject_digest":"`+compliance.SubjectDigest("nobody-here@example.com")+`"`)
		assert.NotContains(t, e.AfterJSON, "nobody-here@example.com")
	})

	t.Run("second factor", func(t *testing.T) {
		rr := post(h.Login, "/api/admin/auth/login", map[string]string{"email": withMFA.Email, "password": password, "tenant": acting})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		challenged := last(t)
		assertEntry(t, challenged, "auth.login.mfa_required", withMFA.ID, true)
		assert.Equal(t, acting, challenged.TenantID)
		var challenge map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &challenge))
		token, _ := challenge["challenge_token"].(string)
		require.NotEmpty(t, token)

		rr = post(h.MFAVerify, "/api/admin/auth/mfa-verify", map[string]string{"challenge_token": token, "code": "000000"})
		require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
		e := last(t)
		assertEntry(t, e, "auth.login.refused", withMFA.ID, false)
		assert.Contains(t, e.AfterJSON, `"step":"second_factor"`)
		assert.NotContains(t, e.AfterJSON, "000000")

		code, err := totp.GenerateCode(totpSecret, time.Now())
		require.NoError(t, err)
		rr = post(h.MFAVerify, "/api/admin/auth/mfa-verify", map[string]string{"challenge_token": token, "code": code})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		e = last(t)
		assertEntry(t, e, "auth.login", withMFA.ID, true)
		assert.Contains(t, e.AfterJSON, `"step":"second_factor"`)
		assert.Equal(t, acting, e.TenantID, "the session's entry is filed where it acts")
		assert.NotContains(t, e.AfterJSON, code)
	})

	t.Run("api token", func(t *testing.T) {
		rr := post(h.Token, "/api/v1/auth/token", map[string]string{"email": plain.Email, "password": password})
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assertEntry(t, last(t), "auth.login", plain.ID, true)

		rr = post(h.Token, "/api/v1/auth/token", map[string]string{"email": plain.Email, "password": "wrong"})
		require.Equal(t, http.StatusUnauthorized, rr.Code)
		assertEntry(t, last(t), "auth.login.refused", plain.ID, false)
	})
}

// The token sign-in is served by the API router, which reaches the audit
// writer only through the host the runtime passes it.
func TestAPIRouter_TokenSignInIsAudited(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	audit := &recordingAuditWriter{}
	compliance.RegisterAuditWriter(audit)
	t.Cleanup(compliance.ClearAuditWriter)

	pool := testdb.Postgres(t)
	const password = "the-audited-password"
	u := seedUser(t, pool, "audited-api@example.com", password, []string{"editor"})
	router, err := NewAPIRouter(pool, testConfig(), nil, WithLifetime(testLifetime(t)), WithScalingHost(auditOnlyHost{}))
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", strings.NewReader(makeLoginBody(u.Email, password)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	e, ok := audit.actions()["auth.login"]
	require.True(t, ok, "no auth.login entry")
	assert.Equal(t, u.ID.String(), e.ResourceID)
}

// The audit writer waits on its caller's context when its queue is full, and
// the sign-in records its entry before the response leaves. The entry is
// written on a context of its own with a short deadline, so a stalled writer
// costs the sign-in that deadline and never holds it.
func TestAuthHandler_AStalledAuditWriterDoesNotHoldTheSignIn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test requires Docker (testcontainers)")
	}
	stalled := &stalledAudit{}
	compliance.RegisterAuditWriter(stalled)
	t.Cleanup(compliance.ClearAuditWriter)

	pool := testdb.Postgres(t)
	const secret = "test-secret-at-least-32-bytes-long!!"
	const password = "the-stalled-password"
	u := seedUser(t, pool, "stalled@example.com", password, []string{"editor"})
	h := NewAuthHandler(db.NewUserStore(pool), &simpleMFAStore{pool: pool, secret: secret}, pool, secret, 3600, false)
	h.host = auditOnlyHost{}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(makeLoginBody(u.Email, password)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.Login(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, int32(1), stalled.calls.Load(), "the sign-in reached the writer")
	assert.Less(t, time.Duration(stalled.held.Load()), time.Second, "the sign-in waited on the audit writer")
}

// stalledAudit stands in for an audit writer whose queue is full: a write
// waits until its caller's context ends, and held is how long the last one
// waited. It gives up after five seconds, so a missing deadline fails the
// test instead of hanging it.
type stalledAudit struct {
	calls atomic.Int32
	held  atomic.Int64
}

func (s *stalledAudit) Writer(ctx context.Context, _ core.Host, _ compliance.AuditEntry) {
	s.calls.Add(1)
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	s.held.Store(int64(time.Since(start)))
}

func (s *stalledAudit) WriterSync(ctx context.Context, h core.Host, e compliance.AuditEntry) error {
	s.Writer(ctx, h, e)
	return ctx.Err()
}
