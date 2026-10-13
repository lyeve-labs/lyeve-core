package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// tenantStreamWriter is a ResponseWriter the stream handler can write to from its
// own goroutine while the test reads what it has sent so far.
type tenantStreamWriter struct {
	mu     sync.Mutex
	header http.Header
	buf    bytes.Buffer
	status int
}

func newTenantStreamWriter() *tenantStreamWriter { return &tenantStreamWriter{header: http.Header{}} }

func (s *tenantStreamWriter) Header() http.Header { return s.header }

func (s *tenantStreamWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		s.status = code
	}
}

func (s *tenantStreamWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.buf.Write(b)
}

func (s *tenantStreamWriter) Flush() {}

func (s *tenantStreamWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// events decodes the data lines sent so far.
func (s *tenantStreamWriter) events(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(s.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &ev), payload)
		out = append(out, ev)
	}
	return out
}

// liveRules is a rule engine an operator edits while a stream is open. It
// stands in for the plugin that owns the rules, whose answer changes the
// moment a rule is written. It exempts super_admin as every checker must, so
// the test's writer is never the one refused.
type liveRules struct {
	read atomic.Bool
	mask atomic.Pointer[[]string]
}

func (c *liveRules) Allowed(_ context.Context, roles []string, _, _ string) (bool, error) {
	if slices.Contains(roles, core.RoleSuperAdmin) {
		return true, nil
	}
	return c.read.Load(), nil
}

func (c *liveRules) FieldMask(context.Context, []string, string) ([]string, error) {
	if m := c.mask.Load(); m != nil {
		return *m, nil
	}
	return nil, nil
}

// openStream starts the stream handler for schema in the given tenant, as a
// caller holding an editor role, and returns once it has registered its hooks.
func openStream(t *testing.T, h *ContentHandler, tenantID, schema string) *tenantStreamWriter {
	t.Helper()
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000001", Roles: []string{"editor"}}
	return openStreamAs(t, h, context.WithValue(tenant.WithID(context.Background(), tenantID), auth.ClaimsKey, claims), schema)
}

// openStreamAs starts the stream handler for schema as the caller on base.
func openStreamAs(t *testing.T, h *ContentHandler, base context.Context, schema string) *tenantStreamWriter {
	t.Helper()
	w, _ := openStreamDone(t, h, base, schema)
	return w
}

// openStreamDone is openStreamAs that also returns a channel closed when the
// handler returns, so a test can see the stream end on its own.
func openStreamDone(t *testing.T, h *ContentHandler, base context.Context, schema string) (*tenantStreamWriter, <-chan struct{}) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/"+schema+"/stream", nil)
	return openStreamRequest(t, h, r.WithContext(base), schema)
}

// openStreamRequest is openStreamDone for a request the test built, such as
// one that carries a credential in a header.
func openStreamRequest(t *testing.T, h *ContentHandler, r *http.Request, schema string) (*tenantStreamWriter, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(r.Context())
	r = chiCtx(r.WithContext(ctx), map[string]string{"schema": schema})

	w := newTenantStreamWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Stream(w, r)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	require.Eventually(t, func() bool {
		return strings.Contains(w.String(), ": connected to "+schema)
	}, 5*time.Second, 10*time.Millisecond, "stream never connected")
	return w, done
}

// Generated tables are shared by every tenant, and the stream's hooks fire on
// the schema name alone. A stream opened in one tenant receives only that
// tenant's writes, with the caller's field mask applied.
func TestContentHandler_Stream_OwnTenantOnly(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{fakeChecker{allow: true, mask: []string{"title"}}}
		stream := openStream(t, fx.content, "t1", "notes")

		other := fx
		other.ctx = asSuperAdmin(tenant.WithID(context.Background(), "t2"))
		createdID(t, other.create(t, "notes", map[string]any{
			"title": "title of t2", "body": "from t2",
		}))
		ownID := createdID(t, fx.create(t, "notes", map[string]any{
			"title": "title of t1", "body": "from t1",
		}))

		require.Eventually(t, func() bool {
			return len(stream.events(t)) > 0
		}, 5*time.Second, 10*time.Millisecond, "the own tenant's write never arrived")
		// The other tenant's write was published first, so had it been sent
		// it would already be in the buffer ahead of this one.
		events := stream.events(t)
		require.Len(t, events, 1, stream.String())
		assert.NotContains(t, stream.String(), "from t2")
		assert.NotContains(t, stream.String(), "title of t2")

		ev := events[0]
		assert.Equal(t, "after_create", ev["event"])
		assert.Equal(t, "notes", ev["schema"])
		data, ok := ev["data"].(map[string]any)
		require.True(t, ok, "data is %T", ev["data"])
		assert.Equal(t, ownID, data["id"])
		assert.Equal(t, "from t1", data["body"])
		assert.NotContains(t, data, "title", "a masked field was streamed")
	})
}

// A caller the read rules refuse gets no stream at all.
func TestContentHandler_Stream_RefusedWithoutRead(t *testing.T) {
	h := &ContentHandler{hooks: hooks.NewRegistry(), perms: fakeProvider{fakeChecker{allow: false}}}
	r := withRoles("editor")
	r = chiCtx(r, map[string]string{"schema": "notes"})
	rr := httptest.NewRecorder()
	h.Stream(rr, r)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.NotContains(t, rr.Body.String(), "connected")
}

// A rule written while a stream is open governs the next event it would send.
// The mask and the read grant were resolved once when the stream opened, so a
// revoked read kept delivering every write until the client reconnected, and
// a field hidden after the open kept streaming.
func TestContentHandler_Stream_RechecksAccessPerEvent(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		rules := &liveRules{}
		rules.read.Store(true)
		fx.content.perms = fakeProvider{rules}
		stream, done := openStreamDone(t, fx.content, asEditor(tenant.WithID(context.Background(), "t1")), "notes")

		createdID(t, fx.create(t, "notes", map[string]any{
			"title": "first title", "body": "before the mask",
		}))
		require.Eventually(t, func() bool { return len(stream.events(t)) == 1 },
			5*time.Second, 10*time.Millisecond, "the first write never arrived")
		first, ok := stream.events(t)[0]["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "first title", first["title"], "the field was hidden before any rule hid it")

		hidden := []string{"title"}
		rules.mask.Store(&hidden)
		createdID(t, fx.create(t, "notes", map[string]any{
			"title": "second title", "body": "after the mask",
		}))
		require.Eventually(t, func() bool { return len(stream.events(t)) == 2 },
			5*time.Second, 10*time.Millisecond, "the second write never arrived")
		second, ok := stream.events(t)[1]["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "after the mask", second["body"])
		assert.NotContains(t, second, "title", "a field masked after the open was streamed")

		rules.read.Store(false)
		createdID(t, fx.create(t, "notes", map[string]any{
			"title": "third title", "body": "after the revoke",
		}))
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream stayed open after its read was revoked")
		}
		// The stream ends on an error event that says why, so a client can
		// tell a revoked read from a dropped connection.
		events := stream.events(t)
		require.Len(t, events, 3, stream.String())
		assert.Contains(t, stream.String(), "event: error\n")
		assert.Equal(t, map[string]any{"error": "permission denied"}, events[2])
		assert.NotContains(t, stream.String(), "after the revoke")
		assert.NotContains(t, stream.String(), "third title")
	})
}

// draftRules lets every role read and lets editor update until the grant is
// taken away, so a test can revoke draft access from an open stream. It
// exempts super_admin, so the test's writer is never the one refused.
type draftRules struct{ update atomic.Bool }

func (c *draftRules) Allowed(_ context.Context, roles []string, _, action string) (bool, error) {
	if slices.Contains(roles, core.RoleSuperAdmin) {
		return true, nil
	}
	if action == core.ActionUpdate {
		return c.update.Load(), nil
	}
	return action == core.ActionRead, nil
}

// Draft access is asked for every change, like the read grant and the mask.
// It was decided once when the stream opened, so an editor whose update grant
// was taken away kept receiving every draft until the client reconnected.
func TestContentHandler_Stream_RechecksDraftAccessPerEvent(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		rules := &draftRules{}
		rules.update.Store(true)
		fx.content.perms = fakeProvider{rules}
		editor := openStreamAs(t, fx.content, asEditor(fx.ctx), "drafts")

		fx.createStatus(t, "drafts", "draft", map[string]any{"title": "before the revoke"})
		require.Eventually(t, func() bool { return len(editor.events(t)) == 1 },
			5*time.Second, 10*time.Millisecond, "the first draft never arrived")

		rules.update.Store(false)
		fx.createStatus(t, "drafts", "draft", map[string]any{"title": "after the revoke"})
		fx.createStatus(t, "drafts", "published", map[string]any{"title": "released"})
		require.Eventually(t, func() bool { return strings.Contains(editor.String(), "released") },
			5*time.Second, 10*time.Millisecond, "the published row never arrived")
		// The draft was written first, so had it been sent it would be ahead
		// of the published row.
		assert.Len(t, editor.events(t), 2, editor.String())
		assert.NotContains(t, editor.String(), "after the revoke")
	})
}

// enableDraftPublish switches draft and publish on for a live schema, the way
// an operator's schema update does. The table gains a status column whose
// default is published, so every row already there stays public, and the
// definition the content path reads gains the flag.
func (fx schemaContentFixture) enableDraftPublish(t *testing.T, schema string) {
	t.Helper()
	add, col := "ADD COLUMN", "VARCHAR(255)"
	if fx.pool.Engine() == "mssql" {
		add, col = "ADD", "NVARCHAR(255)"
	}
	_, err := fx.pool.Exec(context.Background(), fmt.Sprintf(
		"ALTER TABLE %s %s _status %s NOT NULL DEFAULT 'published'", core.TableName(schema), add, col))
	require.NoError(t, err)

	sc, err := fx.eng.GetByName(fx.ctx, schema)
	require.NoError(t, err)
	next := *sc
	next.WithDraftPublish = true
	def, err := json.Marshal(next)
	require.NoError(t, err)
	require.NoError(t, fx.eng.Apply(fx.ctx, schema, def))
}

// Whether the schema has draft and publish is asked for every change, like
// the grants. It was decided once when the stream opened, so after an
// operator switched the mode on, every change still read as published and a
// caller who may not read drafts received each draft written afterwards.
func TestContentHandler_Stream_RechecksDraftModePerEvent(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"posts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		readers := map[string]*tenantStreamWriter{
			"viewer":        openStreamAs(t, fx.content, asReader(fx.ctx), "posts"),
			"read-only key": openStreamAs(t, fx.content, asKey("content.posts:read"), "posts"),
		}
		editor := openStreamAs(t, fx.content, asEditor(fx.ctx), "posts")

		createdID(t, fx.create(t, "posts", map[string]any{"title": "before the switch"}))
		fx.enableDraftPublish(t, "posts")
		fx.createStatus(t, "posts", "draft", map[string]any{"title": "draft after the switch"})
		fx.createStatus(t, "posts", "published", map[string]any{"title": "published after the switch"})

		// The editor may read drafts, so its stream shows the draft was
		// written as one and reached the hooks.
		require.Eventually(t, func() bool { return len(editor.events(t)) == 3 },
			5*time.Second, 10*time.Millisecond, editor.String())
		assert.Contains(t, editor.String(), "draft after the switch")

		for name, stream := range readers {
			require.Eventually(t, func() bool { return strings.Contains(stream.String(), "published after the switch") },
				5*time.Second, 10*time.Millisecond, "%s: the published row never arrived", name)
			// The draft was written first, so had it been sent it would be
			// ahead of the published row.
			assert.Len(t, stream.events(t), 2, "%s: %s", name, stream.String())
			assert.NotContains(t, stream.String(), "draft after the switch", name)
		}
	})
}

// failingSchemas is a schema source that stops answering on demand, the way
// a schema store does when its database goes away while a stream is open.
type failingSchemas struct {
	core.SchemaSource
	down atomic.Bool
}

func (s *failingSchemas) GetByName(ctx context.Context, name string) (*core.Schema, error) {
	if s.down.Load() {
		return nil, errors.New("schema store unavailable")
	}
	return s.SchemaSource.GetByName(ctx, name)
}

// A schema whose definition cannot be read counts as having draft and
// publish, so a change without a status is withheld from a caller who may not
// read drafts rather than sent on the mode the stream saw when it opened.
func TestContentHandler_Stream_UnreadableSchemaWithholdsChanges(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"posts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		// The writer reads the schema from the store itself, so only the
		// stream's lookup fails.
		writer := fx
		writer.content = &ContentHandler{store: fx.content.store, schemas: fx.content.schemas, hooks: fx.content.hooks, perms: fx.content.perms}
		src := &failingSchemas{SchemaSource: fx.content.schemas}
		fx.content.schemas = src
		viewer := openStreamAs(t, fx.content, asReader(fx.ctx), "posts")
		editor := openStreamAs(t, fx.content, asEditor(fx.ctx), "posts")

		src.down.Store(true)
		createdID(t, writer.create(t, "posts", map[string]any{"title": "while the schema is unreadable"}))
		require.Eventually(t, func() bool { return len(editor.events(t)) == 1 },
			5*time.Second, 10*time.Millisecond, editor.String())

		src.down.Store(false)
		createdID(t, writer.create(t, "posts", map[string]any{"title": "once the schema reads again"}))
		require.Eventually(t, func() bool { return strings.Contains(viewer.String(), "once the schema reads again") },
			5*time.Second, 10*time.Millisecond, viewer.String())
		assert.Len(t, viewer.events(t), 1, viewer.String())
		assert.NotContains(t, viewer.String(), "while the schema is unreadable")
	})
}

// The error event that ends a stream names the refusal in the words the
// content routes answer with, and a rule store's failure never reaches the
// client as its own text.
func TestStreamEndReason_NamesTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errNoCaller, "authentication required"},
		{errSchemaNotAllowed, "schema not allowed for this credential"},
		{errScopeDenied, "insufficient scope"},
		{fmt.Errorf("permission check notes: %w", errPermissionDenied), "permission denied"},
		{errors.New("dial tcp 10.0.0.5:5432: connection refused"), "access could not be checked"},
	} {
		assert.Equal(t, tc.want, streamEndReason(tc.err), tc.err.Error())
	}
}
