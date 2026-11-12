package api

import (
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
)

// net/http parses the method and path as substrings of the request line, so
// any log entry that keeps them as they arrived keeps the whole line, query
// string included. This sends long request lines through the access log, the
// enrich handler, the tailer and the interceptor the way the runtime chains
// them, and checks the rings did not keep the lines.
func TestAccessLog_LongRequestLinesAreNotRetained(t *testing.T) {
	const (
		requests  = 1000
		querySize = 256 << 10
	)

	ring := logstream.NewBuffer(logstream.DefaultBufferSize)
	enrich := logging.NewEnrichHandler(logstream.NewInterceptor(slog.NewJSONHandler(io.Discard, nil), ring), nil)
	previous, logOut, logFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(enrich))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(logOut)
		log.SetFlags(logFlags)
	})
	previousTailer := observability.GlobalTailer()
	observability.SetGlobalTailer(observability.NewLogTailer())
	t.Cleanup(func() { observability.SetGlobalTailer(previousTailer) })

	r := chi.NewRouter()
	r.Use(structuredLogger)
	r.Get("/items", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	heapInUse := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapInuse
	}

	before := heapInUse()
	query := strings.Repeat("a", querySize)
	for range requests {
		req := httptest.NewRequest(http.MethodGet, "/items?q="+query, nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
	after := heapInUse()

	var grown uint64
	if after > before {
		grown = after - before
	}
	t.Logf("heap in use grew by %d bytes over %d requests with %d byte queries", grown, requests, querySize)
	if got := len(ring.Snapshot(nil, 0)); got < requests {
		t.Fatalf("ring holds %d entries, want at least %d", got, requests)
	}
	// One request line is 256 KiB, so keeping them all would be about 256 MiB.
	// The entries themselves come to well under a MiB.
	if grown > 16<<20 {
		t.Fatalf("heap in use grew by %d bytes: the log entries keep the request lines", grown)
	}
}
