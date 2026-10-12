package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

type discardCapture struct{}

func (discardCapture) Capture(observability.CaptureEntry) error { return nil }

// The API listener gives a response a fixed time to be written, set once when
// the request is read. A plugin's stream outlives it only by moving its own
// write deadline through http.ResponseController, and that reaches the
// connection only when every writer the router wraps a response in unwraps to
// the one below it. One wrapper without Unwrap ends every stream on the router
// at the server's write timeout again, with nothing in the plugin to show why.
//
// This mounts a stream that renews its deadline on each write, as the realtime
// stream does, behind the API router with request capture and API key audit
// switched on, and reads an event written after the server's deadline.
func TestAPIRouter_StreamOutlivesTheServerWriteTimeout(t *testing.T) {
	t.Parallel()

	events := make(chan string)
	refused := make(chan error, 1)
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		hold := func() bool {
			if err := rc.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
				refused <- err
				return false
			}
			return true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if !hold() {
			return
		}
		_, _ = fmt.Fprint(w, ":ok\n\n")
		_ = rc.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-events:
				if !hold() {
					return
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", e)
				_ = rc.Flush()
			}
		}
	})
	routes := []plugin.PluginRoutes{{
		Name:   "widgets",
		Routes: []plugin.RouteDecl{{Method: http.MethodGet, Pattern: "/api/v1/widgets/stream", Group: plugin.GroupAuth, Handler: stream}},
	}}

	router, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil,
		WithLifetime(testLifetime(t)),
		WithAPIKeyAuth(injectTestClaims),
		WithAPIKeyAudit(apimw.APIKeyAudit(func(string, string, string, int, string) {}, nil)),
		WithRequestCapture(discardCapture{}),
		WithPluginRoutes(routes))
	if err != nil {
		t.Fatalf("NewAPIRouter: %v", err)
	}

	const writeTimeout = time.Second
	srv := httptest.NewUnstartedServer(NewSwappableHandler(router))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/widgets/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Scopes", "widgets:read")
	sent := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open the stream: status %d", resp.StatusCode)
	}

	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() != "" {
				lines <- sc.Text()
			}
		}
	}()
	next := func(want string) {
		t.Helper()
		select {
		case err := <-refused:
			t.Fatalf("the router's writers do not reach the connection: SetWriteDeadline: %v", err)
		case got, ok := <-lines:
			if !ok {
				t.Fatalf("the stream ended before %q arrived", want)
			}
			if got != want {
				t.Fatalf("read %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%q did not arrive", want)
		}
	}

	next(":ok")
	time.Sleep(writeTimeout + 300*time.Millisecond - time.Since(sent))
	select {
	case events <- "after the deadline":
	case <-time.After(3 * time.Second):
		t.Fatal("the stream stopped taking events")
	}
	next("data: after the deadline")
}
