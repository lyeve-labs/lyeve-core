package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A trace backend groups spans by name, and an exporter queues every span it
// has not sent yet. A span named by its path makes one group per identifier
// and carries the identifier, and a span attribute taken from the request as
// parsed keeps the whole request line, query string included.
func TestHTTPMiddleware_SpanNamesTheRouteAndClipsTheURL(t *testing.T) {
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())
	defer resetTracing()

	r := chi.NewRouter()
	r.Use(HTTPMiddleware("test-service"))
	r.Get("/items/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	long := "/items/" + strings.Repeat("a", 4096)
	for _, tc := range []struct {
		name, method, target string
		wantName, wantMethod string
		wantPath             string
	}{
		{"routed", http.MethodGet, "/items/42?token=secret", "GET /items/{id}", "GET", "/items/42"},
		{"unrouted", http.MethodGet, "/no/such/route?token=secret", "GET", "GET", "/no/such/route"},
		{"nonstandard method", "FOOBAR", "/no/such/route", "HTTP", "_OTHER", "/no/such/route"},
		{"long path", http.MethodGet, long, "GET /items/{id}", "GET", long[:maxSpanURLBytes]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exp.Reset()
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.target, nil))

			spans := exp.GetSpans()
			require.Len(t, spans, 1)
			assert.Equal(t, tc.wantName, spans[0].Name)
			attrs := spanAttrs(spans[0])
			assert.Equal(t, tc.wantMethod, attrs["http.request.method"])
			assert.Equal(t, tc.wantPath, attrs["url.path"])
			for k, v := range attrs {
				assert.NotContains(t, v, "token=secret", "attribute %q carries the query string", k)
			}
		})
	}
}

func TestHTTPMiddleware_SpansDoNotKeepTheRequestLine(t *testing.T) {
	const (
		requests  = 1000
		querySize = 256 << 10
	)
	exp, tp := testTraceProvider()
	defer tp.Shutdown(context.Background())
	defer resetTracing()

	r := chi.NewRouter()
	r.Use(HTTPMiddleware("test-service"))
	r.Get("/items/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

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
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/42?q="+query, nil))
	}
	after := heapInUse()
	require.Len(t, exp.GetSpans(), requests, "the exporter keeps every span")

	var grown uint64
	if after > before {
		grown = after - before
	}
	t.Logf("heap in use grew by %d bytes over %d requests with %d byte queries", grown, requests, querySize)
	// One request line is 256 KiB, so keeping them all would be about 256 MiB.
	assert.LessOrEqual(t, grown, uint64(16<<20), "the spans keep the request lines")
}
