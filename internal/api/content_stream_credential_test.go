package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// streamChange publishes one change to schema in tenant t1, the way a write
// does, without a content store.
func streamChange(t *testing.T, h *ContentHandler, schema, title string) {
	t.Helper()
	require.NoError(t, h.hooks.Run(tenant.WithID(context.Background(), "t1"),
		hooks.Event{Type: hooks.AfterUpdate, Schema: schema, Data: map[string]any{"title": title}}))
}

// requireEndedByCredential waits for a stream to end on its own and checks
// that it said why.
func requireEndedByCredential(t *testing.T, stream *tenantStreamWriter, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("the stream stayed open after its credential ended: %s", stream.String())
	}
	assert.Contains(t, stream.String(), "event: error\n")
	assert.Contains(t, stream.String(), `data: {"error":"credential expired or revoked"}`)
}

// A stream authenticated once, when it opened, and a heartbeat every few
// seconds kept it open for as long as the client stayed. A token past its
// expiry went on receiving every change. The stream ends when its token
// expires, with no change needed to notice it.
func TestContentHandler_Stream_EndsWhenItsTokenExpires(t *testing.T) {
	h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}}}
	claims := &auth.Claims{UserID: uuid.NewString(), Roles: []string{"editor"}, TokenType: "session"}
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(2 * time.Second))
	stream, done := openStreamDone(t, h, context.WithValue(tenant.WithID(context.Background(), "t1"), auth.ClaimsKey, claims), "notes")

	streamChange(t, h, "notes", "before the expiry")
	require.Eventually(t, func() bool { return len(stream.events(t)) == 1 },
		5*time.Second, 10*time.Millisecond, "a change before the expiry never arrived")
	requireEndedByCredential(t, stream, done)
}

// Every way an account's sessions end raises its token version or removes
// it, and a request after that is refused. An open stream is held to the
// same check at its next change, on every dialect.
func TestContentHandler_Stream_EndsWhenItsSessionEnds(t *testing.T) {
	for _, d := range []struct {
		name string
		pool func(*testing.T) db.DB
	}{
		{"postgres", testdb.Postgres},
		{"mysql", testdb.MySQL},
		{"mssql", testdb.MSSQL},
	} {
		t.Run(d.name, func(t *testing.T) {
			if !testdb.ShouldTest(d.name) {
				t.Skipf("CI_DIALECT != %s", d.name)
			}
			users := db.NewUserStore(d.pool(t))
			for _, end := range []struct {
				name string
				do   func(context.Context, uuid.UUID) error
			}{
				{"sign-out", users.BumpTokenVersion},
				{"password set by a super admin", func(ctx context.Context, id uuid.UUID) error {
					_, err := users.SetPassword(ctx, id, "$2a$10$anotherhashforthestreamtestXXXXXXXXXXXXXXXXXXXXXXXX")
					return err
				}},
				{"account disabled", func(ctx context.Context, id uuid.UUID) error {
					off := true
					_, err := users.SetAccountState(ctx, id, &off, nil)
					return err
				}},
				{"account deleted", users.Delete},
			} {
				t.Run(end.name, func(t *testing.T) {
					ctx := context.Background()
					u, err := users.Create(ctx, uuid.NewString()+"@stream.test", "!hash!", []string{"editor"}, "t1")
					require.NoError(t, err)
					t.Cleanup(func() { _ = users.Delete(ctx, u.ID) })
					h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}},
						streamCredential: streamCredential(tokenVersionGetter(users), nil)}
					claims := &auth.Claims{UserID: u.ID.String(), Roles: u.Roles, TokenType: "session", TokenVersion: u.TokenVersion}
					stream, done := openStreamDone(t, h, context.WithValue(tenant.WithID(ctx, "t1"), auth.ClaimsKey, claims), "notes")

					streamChange(t, h, "notes", "while signed in")
					require.Eventually(t, func() bool { return len(stream.events(t)) == 1 },
						5*time.Second, 10*time.Millisecond, "a change in a live session never arrived")

					require.NoError(t, end.do(ctx, u.ID))
					streamChange(t, h, "notes", "after the session ended")
					requireEndedByCredential(t, stream, done)
					assert.NotContains(t, stream.String(), "after the session ended")
				})
			}
		})
	}
}

// A key goes back through the key middleware before each change, so a key
// revoked while its stream is open stops receiving at the next one, and so
// does a key whose scopes were narrowed. The key store belongs to the plugin
// that issues keys, so this one is a stand-in that answers for one key on
// the terms it holds now.
func TestContentHandler_Stream_EndsWhenItsKeyIsRevoked(t *testing.T) {
	const raw = "stream-test-key"
	opened := core.AuthClaims{UserID: "key-1", APIKeyID: "key-1", Roles: []string{"editor"}, Scopes: []string{"content:read"}, IsAPIKey: true}
	for _, tc := range []struct {
		name   string
		change func(*core.AuthClaims) *core.AuthClaims
	}{
		{"revoked", func(*core.AuthClaims) *core.AuthClaims { return nil }},
		{"scopes narrowed", func(c *core.AuthClaims) *core.AuthClaims {
			c.Scopes = []string{"content.other:read"}
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed atomic.Bool
			lookup := func(_ context.Context, hash string) (*core.AuthClaims, error) {
				if hash != security.HashKeyPeppered(raw) {
					return nil, nil
				}
				now := opened
				if changed.Load() {
					return tc.change(&now), nil
				}
				return &now, nil
			}
			h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: true}},
				streamCredential: streamCredential(nil, apimw.APIKeyAuth(lookup))}
			key := opened
			r := httptest.NewRequest(http.MethodGet, "/api/v1/content/notes/stream", nil)
			r.Header.Set("X-API-Key", raw)
			r = r.WithContext(context.WithValue(tenant.WithID(context.Background(), "t1"), core.ClaimsKey, &key))
			stream, done := openStreamRequest(t, h, r, "notes")

			streamChange(t, h, "notes", "while the key holds")
			require.Eventually(t, func() bool { return len(stream.events(t)) == 1 },
				5*time.Second, 10*time.Millisecond, "a change for a live key never arrived")

			changed.Store(true)
			streamChange(t, h, "notes", "after the change")
			requireEndedByCredential(t, stream, done)
			assert.NotContains(t, stream.String(), "after the change")
		})
	}
}
