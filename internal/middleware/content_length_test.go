package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContentLengthLimit(t *testing.T) {
	const maxBytes = 1024 // 1 KiB

	handler := ContentLengthLimit(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// MaxBytesReader limits body reads. Error is expected when
			// the limit is hit. Write 413 explicitly for clarity.
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("X-Read", string(body))
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		reqFunc    func() *http.Request
		wantStatus int
		wantBody   string // expected response body substring (empty = don't check)
	}{
		{
			name: "content_length_under_limit_passes",
			reqFunc: func() *http.Request {
				return httptest.NewRequest(http.MethodPost, "/", strings.NewReader("small body"))
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "content_length_over_limit_rejected_413",
			reqFunc: func() *http.Request {
				big := strings.NewReader(strings.Repeat("x", maxBytes+1))
				req := httptest.NewRequest(http.MethodPost, "/", big)
				return req
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			// The refusal is the JSON envelope, like every other response.
			wantBody: `"code":"payload_too_large"`,
		},
		{
			name: "chunked_body_under_limit_passes",
			reqFunc: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("small"))
				req.ContentLength = -1 // simulate chunked encoding
				return req
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "chunked_body_over_limit_capped",
			reqFunc: func() *http.Request {
				big := strings.NewReader(strings.Repeat("y", maxBytes+512))
				req := httptest.NewRequest(http.MethodPost, "/", big)
				req.ContentLength = -1 // simulate chunked: no Content-Length header
				return req
			},
			wantStatus: http.StatusRequestEntityTooLarge, // handler writes 413 on read error
		},
		{
			name: "no_content_length_over_limit_capped",
			reqFunc: func() *http.Request {
				// Use a pipe so the content length is genuinely unknown.
				pr, pw := io.Pipe()
				go func() {
					_, _ = io.Copy(pw, strings.NewReader(strings.Repeat("z", maxBytes+1)))
					pw.Close()
				}()
				req := httptest.NewRequest(http.MethodPost, "/", pr)
				req.ContentLength = 0 // Go treats 0 as "unknown" for piped bodies
				return req
			},
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, tt.reqFunc())
			assert.Equal(t, tt.wantStatus, w.Code, "status mismatch")
			if tt.wantBody != "" {
				assert.Contains(t, w.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestContentLengthLimit_BodyActuallyCapped(t *testing.T) {
	// Verify MaxBytesReader limits the bytes a handler can read from
	// chunked requests, even without a real HTTP server's 413 machinery.
	const maxBytes = 512

	var bytesRead atomic.Int64

	handler := ContentLengthLimit(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		bytesRead.Store(n)
	}))

	big := strings.NewReader(strings.Repeat("x", maxBytes*4))
	req := httptest.NewRequest(http.MethodPost, "/", big)
	req.ContentLength = -1 // simulate chunked
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// MaxBytesReader caps at maxBytes. Handler can't read the full 4x payload.
	assert.LessOrEqual(t, bytesRead.Load(), int64(maxBytes),
		"handler should not read more than maxBytes from a chunked request")
}

func TestContentLengthLimit_Disabled(t *testing.T) {
	handler := ContentLengthLimit(0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Read", string(body))
		w.WriteHeader(http.StatusOK)
	}))

	// Huge body with limit=0 should pass through unmodified.
	big := strings.Repeat("x", 1<<20)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, w.Header().Get("X-Read"), 1<<20)
}

func TestContentLengthLimit_NilBody(t *testing.T) {
	handler := ContentLengthLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) // nil body
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
