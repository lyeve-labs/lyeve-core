package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
)

func TestCorrelationID_GeneratesWhenAbsent(t *testing.T) {
	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := CorrelationIDFromCtx(r.Context())
		assert.NotEmpty(t, id, "should generate an ID")
		assert.Len(t, id, 32, "should be 128-bit hex (32 chars)")

		chiID := middleware.GetReqID(r.Context())
		assert.Equal(t, id, chiID, "should match chi request ID")

		logID := logging.RequestIDFromCtx(r.Context())
		assert.Equal(t, id, logID, "should match logging request ID")

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"), "response should echo X-Request-ID")
}

func TestCorrelationID_AcceptsInboundRequestID(t *testing.T) {
	const inboundID = "my-custom-request-id-123"

	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := CorrelationIDFromCtx(r.Context())
		assert.Equal(t, inboundID, id, "should use inbound X-Request-ID")

		chiID := middleware.GetReqID(r.Context())
		assert.Equal(t, inboundID, chiID)

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", inboundID)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, inboundID, rec.Header().Get("X-Request-ID"))
}

// TestCorrelationID_RefusesAnUnusableInboundID covers an inbound X-Request-ID
// that must not be adopted.
//
// The header is echoed into the response and into every log line for the
// request, so whatever arrives is amplified. A 16 KB value would come back as
// a 16 KB response header, which overruns the header budget most clients
// allocate, so the caller could not parse the response and would read it as a
// transport failure rather than as its own doing.
func TestCorrelationID_RefusesAnUnusableInboundID(t *testing.T) {
	tests := []struct {
		name    string
		inbound string
	}{
		{"oversized", strings.Repeat("A", 16*1024)},
		{"one past the bound", strings.Repeat("A", maxCorrelationIDLen+1)},
		{"carriage return", "abc\r\nSet-Cookie: x=1"},
		{"newline", "abc\nX-Injected: 1"},
		{"null byte", "abc\x00def"},
		{"non-ascii", "abc\u00e9def"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = CorrelationIDFromCtx(r.Context())
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("X-Request-ID", tc.inbound)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			assert.NotEqual(t, tc.inbound, seen, "the inbound value must not be adopted")
			assert.NotEmpty(t, seen, "a replacement ID must still be issued")

			echoed := rec.Header().Get("X-Request-ID")
			assert.Equal(t, seen, echoed, "the echoed header must match the resolved ID")
			assert.LessOrEqual(t, len(echoed), maxCorrelationIDLen, "the echoed header must be bounded")
		})
	}
}

// TestCorrelationID_AcceptsAnIDAtTheBound pins the boundary itself, so a change
// to maxCorrelationIDLen has to be deliberate.
func TestCorrelationID_AcceptsAnIDAtTheBound(t *testing.T) {
	inbound := strings.Repeat("a", maxCorrelationIDLen)

	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, inbound, CorrelationIDFromCtx(r.Context()))
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", inbound)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, inbound, rec.Header().Get("X-Request-ID"))
}

func TestCorrelationID_AcceptsTraceparent(t *testing.T) {
	// W3C traceparent format: version-traceid-parentid-traceflags.
	const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	const expectedTraceID = "0af7651916cd43dd8448eb211c80319c"

	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := CorrelationIDFromCtx(r.Context())
		assert.Equal(t, expectedTraceID, id, "should extract trace-id from traceparent")

		tp := traceparentFromCtx(r.Context())
		assert.Equal(t, traceparent, tp, "should store raw traceparent")

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("traceparent", traceparent)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, expectedTraceID, rec.Header().Get("X-Request-ID"))
}

func TestCorrelationID_PrefersXRequestIDOverTraceparent(t *testing.T) {
	const inboundID = "explicit-request-id"
	const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := CorrelationIDFromCtx(r.Context())
		assert.Equal(t, inboundID, id, "X-Request-ID should take priority over traceparent")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", inboundID)
	req.Header.Set("traceparent", traceparent)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, inboundID, rec.Header().Get("X-Request-ID"))
}

func TestCorrelationID_IgnoresInvalidTraceparent(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
	}{
		{"empty", ""},
		{"too few parts", "00-0af7651916cd43dd8448eb211c80319c"},
		{"too many parts", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-extra"},
		{"short trace-id", "00-0af765-b7ad6b7169203331-01"},
		{"all-zero trace-id", "00-00000000000000000000000000000000-b7ad6b7169203331-01"},
		{"short parent-id", "00-0af7651916cd43dd8448eb211c80319c-b7ad-01"},
		{"short flags", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := CorrelationIDFromCtx(r.Context())
				assert.NotEmpty(t, id, "should generate a new ID")
				assert.Len(t, id, 32, "should be 128-bit hex")
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tt.traceparent != "" {
				req.Header.Set("traceparent", tt.traceparent)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
		})
	}
}

func TestCorrelationID_ResponseHeaderAlwaysSet(t *testing.T) {
	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
}

func TestCorrelationID_Uniqueness(t *testing.T) {
	handler := CorrelationID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ids := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		id := rec.Header().Get("X-Request-ID")
		assert.NotEmpty(t, id)
		assert.False(t, ids[id], "duplicate ID generated: %s", id)
		ids[id] = true
	}
}

func TestExtractTraceID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "valid traceparent",
			input:    "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			expected: "0af7651916cd43dd8448eb211c80319c",
		},
		{
			name:     "all-zero trace-id is invalid",
			input:    "00-00000000000000000000000000000000-b7ad6b7169203331-01",
			expected: "",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "too few parts",
			input:    "00-0af7651916cd43dd8448eb211c80319c",
			expected: "",
		},
		{
			name:     "short trace-id",
			input:    "00-0af765-b7ad6b7169203331-01",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTraceID(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCorrelationIDFromCtx_NotSet(t *testing.T) {
	assert.Equal(t, "", CorrelationIDFromCtx(nil)) //nolint:staticcheck // intentionally testing nil-context guard
	assert.Equal(t, "", CorrelationIDFromCtx(context.Background()))
}

func TestTraceparentFromCtx_NotSet(t *testing.T) {
	assert.Equal(t, "", traceparentFromCtx(nil)) //nolint:staticcheck // intentionally testing nil-context guard
	assert.Equal(t, "", traceparentFromCtx(context.Background()))
}
