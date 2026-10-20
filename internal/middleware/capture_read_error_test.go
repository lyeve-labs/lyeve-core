package middleware_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// failingBody hands out its bytes, then fails the way a dropped connection or
// a broken chunked encoding does partway through an upload.
type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *failingBody) Close() error { return nil }

// A body that fails partway must never reach the handler as a shorter body.
// The handler would parse what it got and blame the client: a media upload
// cut short answered 400 "invalid multipart form" for a form the client sent
// whole, and the only log line was at debug.
func TestRequestCapture_FailedBodyReadNeverReachesTheHandler(t *testing.T) {
	form := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.svg\"\r\n" +
		"Content-Type: image/svg+xml\r\n\r\n" + strings.Repeat("x", 120) + "\r\n--b--\r\n"
	const sent = 100

	cases := []struct {
		name   string
		body   func(w http.ResponseWriter) io.ReadCloser
		status int
	}{
		{
			name: "a broken connection",
			body: func(http.ResponseWriter) io.ReadCloser {
				return &failingBody{data: []byte(form[:sent]), err: errors.New("connection reset by peer")}
			},
			status: http.StatusBadRequest,
		},
		{
			name: "a body over the size limit",
			body: func(w http.ResponseWriter) io.ReadCloser {
				return http.MaxBytesReader(w, io.NopCloser(strings.NewReader(form)), sent)
			},
			status: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(prev)

			reached := false
			h := middleware.RequestCaptureWithPolicy(&testSink{}, nil, nil)(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reached = true
					w.WriteHeader(http.StatusNoContent)
				}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/media", nil)
			req.Body = tc.body(rec)
			req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			req = req.WithContext(context.WithValue(req.Context(), chimw.RequestIDKey, "req-7"))
			h.ServeHTTP(rec, req)

			assert.False(t, reached, "the handler must not be served a body that failed to read")
			assert.Equal(t, tc.status, rec.Code)
			require.Contains(t, logs.String(), `"level":"WARN"`)
			assert.Contains(t, logs.String(), `"request_id":"req-7"`)
			assert.Contains(t, logs.String(), `"bytes_read":100`)
		})
	}
}
