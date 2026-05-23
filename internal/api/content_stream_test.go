package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
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

// openStream starts the stream handler for schema in the given tenant, as a
// caller holding an editor role, and returns once it has registered its hooks.
func openStream(t *testing.T, h *ContentHandler, tenantID, schema string) *tenantStreamWriter {
	t.Helper()
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenantID))
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000001", Roles: []string{"editor"}}
	ctx = context.WithValue(ctx, auth.ClaimsKey, claims)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/"+schema+"/stream", nil)
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
	return w
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
