package middleware

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

type finalizedWriter struct {
	*httptest.ResponseRecorder
}

// A pooled encoder that still points at a finished response keeps that
// response reachable while the encoder sits in the pool. One collection moves
// the pool's contents to its victim cache without freeing them, so a response
// that is collected after one GC was not held by the pool.
func TestCompress_PooledWriterReleasesTheResponse(t *testing.T) {
	for _, enc := range []string{"gzip", "br"} {
		t.Run(enc, func(t *testing.T) {
			h := CompressWithLevelFunc(func(string, int) (int, int) { return 2, 2 })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))

			collected := serveTracked(h, enc)
			runtime.GC()
			select {
			case <-collected:
			case <-time.After(2 * time.Second):
				t.Fatal("the response writer is still reachable after the request finished")
			}
		})
	}
}

func serveTracked(h http.Handler, enc string) <-chan struct{} {
	collected := make(chan struct{})
	w := &finalizedWriter{httptest.NewRecorder()}
	runtime.SetFinalizer(w, func(*finalizedWriter) { close(collected) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", enc)
	h.ServeHTTP(w, req)
	if w.Header().Get("Content-Encoding") != enc {
		panic("response was not compressed with " + enc)
	}
	return collected
}
