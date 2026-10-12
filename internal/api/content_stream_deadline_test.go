package api

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The API listener gives a response 15 seconds to be written, and net/http
// sets that deadline once, when it reads the request. The change stream left
// it alone, so the first change written after it was lost and the stream
// ended. This serves the stream behind a server with a one second write
// timeout, through writer wrappers shaped like the router's, and reads
// heartbeats across the deadline, then a change and a heartbeat after it.
func TestContentHandler_StreamOutlivesTheServerWriteTimeout(t *testing.T) {
	t.Parallel()

	h := &ContentHandler{
		hooks:              hooks.NewRegistry(),
		perms:              fakeProvider{fakeChecker{allow: true}},
		streamHeartbeat:    200 * time.Millisecond,
		streamDeadlineWarn: new(sync.Once),
	}

	const writeTimeout = time.Second
	srv := httptest.NewUnstartedServer(asStreamReader("t1", wrapLikeTheRouter(contentStreamRoute(h))))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	sent := time.Now()
	lines := openChangeStream(t, srv.URL+"/api/v1/content/notes/stream")
	readStreamLine(t, lines, ": connected to notes stream")

	// Heartbeats carry the stream across the server's deadline.
	for time.Since(sent) < writeTimeout+300*time.Millisecond {
		readStreamLine(t, lines, ": heartbeat")
	}

	publishNote(t, h, "t1", "after the deadline")
	readStreamLine(t, lines, `"body":"after the deadline"`)
	readStreamLine(t, lines, ": heartbeat")
}

// A chain that cannot move the deadline still serves the stream, as it did
// before the stream held its own, and says so once rather than once per
// stream. It replaces the default logger, so it does not run in parallel.
func TestContentHandler_StreamWarnsOnceWhenTheDeadlineCannotMove(t *testing.T) {
	var logs lockedLogBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := &ContentHandler{
		hooks:              hooks.NewRegistry(),
		perms:              fakeProvider{fakeChecker{allow: true}},
		streamDeadlineWarn: new(sync.Once),
	}
	srv := httptest.NewServer(asStreamReader("t1", hideTheConnection(contentStreamRoute(h))))
	t.Cleanup(srv.Close)

	first := openChangeStream(t, srv.URL+"/api/v1/content/notes/stream")
	readStreamLine(t, first, ": connected to notes stream")
	second := openChangeStream(t, srv.URL+"/api/v1/content/notes/stream")
	readStreamLine(t, second, ": connected to notes stream")

	publishNote(t, h, "t1", "to both")
	readStreamLine(t, first, `"body":"to both"`)
	readStreamLine(t, second, `"body":"to both"`)

	require.Equal(t, 1, strings.Count(logs.String(), "cannot extend their write deadline"),
		"one warning for the chain, not one per stream: %s", logs.String())
}

// contentStreamRoute mounts the stream at the path the API router gives it.
func contentStreamRoute(h *ContentHandler) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/content/{schema}/stream", h.Stream)
	return r
}

// asStreamReader resolves an editor in the tenant the way the router's
// authentication middleware does before the handler runs.
func asStreamReader(tenantID string, next http.Handler) http.Handler {
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000001", Roles: []string{"editor"}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(tenant.WithID(r.Context(), tenantID), auth.ClaimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// publishNote publishes a created note in the tenant the way a write does.
func publishNote(t *testing.T, h *ContentHandler, tenantID, body string) {
	t.Helper()
	ctx := tenant.WithID(context.Background(), tenantID)
	require.NoError(t, core.PublishContentEvent(ctx, h.hooks, core.SourceEngine, core.AfterCreate,
		"notes", "00000000-0000-0000-0000-0000000000aa", nil, map[string]any{"body": body}))
}

// deadlineStatusWriter has the shape of every writer the router wraps a
// response in: it embeds the writer, records the status, and forwards Flush
// and Unwrap.
type deadlineStatusWriter struct {
	http.ResponseWriter
	status int
}

func (s *deadlineStatusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *deadlineStatusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *deadlineStatusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// wrapLikeTheRouter puts two wrappers between the server and the handler, as
// the access log and the compression middleware do.
func wrapLikeTheRouter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&deadlineStatusWriter{ResponseWriter: &deadlineStatusWriter{ResponseWriter: w}}, r)
	})
}

// opaqueStreamWriter flushes but does not unwrap, so nothing below it can
// reach the connection.
type opaqueStreamWriter struct {
	http.ResponseWriter
}

func (o *opaqueStreamWriter) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func hideTheConnection(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&opaqueStreamWriter{ResponseWriter: w}, r)
	})
}

// openChangeStream opens the stream and returns its lines as they arrive.
// The channel closes when the response ends.
func openChangeStream(t *testing.T, url string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sc.Text()
			}
		}
	}()
	return lines
}

// readStreamLine reads lines until one contains want. It fails when the
// stream ends first.
func readStreamLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case got, ok := <-lines:
			if !ok {
				t.Fatalf("the stream ended before %q arrived", want)
			}
			if strings.Contains(got, want) {
				return
			}
		case <-timeout:
			t.Fatalf("%q did not arrive", want)
		}
	}
}

// lockedLogBuffer is a log destination the test can read while streams
// write.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
