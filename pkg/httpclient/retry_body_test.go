package httpclient

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyRecorder answers the first request with failFirst and every later one
// with 200, and keeps the body each attempt carried.
type bodyRecorder struct {
	mu        sync.Mutex
	bodies    []string
	failFirst func(w http.ResponseWriter)
}

func (b *bodyRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.bodies = append(b.bodies, string(got))
	n := len(b.bodies)
	b.mu.Unlock()
	if n == 1 {
		b.failFirst(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (b *bodyRecorder) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.bodies...)
}

func answer503(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }

// streamBody hides the concrete reader, so http.NewRequest cannot set
// GetBody and the client has to buffer the body itself.
type streamBody struct{ r io.Reader }

func (s streamBody) Read(p []byte) (int, error) { return s.r.Read(p) }

func TestDoRequest_RetryResendsTheBody(t *testing.T) {
	const payload = `{"event":"audit.entry","seq":42}`

	cases := []struct {
		name      string
		body      func() io.Reader
		failFirst func(w http.ResponseWriter)
	}{
		{"rewindable reader after a 503", func() io.Reader { return bytes.NewReader([]byte(payload)) }, answer503},
		{"stream after a 503", func() io.Reader { return streamBody{strings.NewReader(payload)} }, answer503},
		{"stream after a 429", func() io.Reader { return streamBody{strings.NewReader(payload)} }, func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &bodyRecorder{failFirst: tc.failFirst}
			srv := httptest.NewServer(rec)
			defer srv.Close()

			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, tc.body())
			require.NoError(t, err)

			resp, err := DoRequest(context.Background(), req,
				WithRetries(2),
				WithRetryDelay(10*time.Millisecond),
				WithSkipSSRF(true),
			)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, []string{payload, payload}, rec.seen())
		})
	}
}

func TestDoRequest_RetryAfterDroppedConnectionResendsTheBody(t *testing.T) {
	const payload = "sink batch 7"
	rec := &bodyRecorder{failFirst: func(w http.ResponseWriter) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, streamBody{strings.NewReader(payload)})
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req,
		WithRetries(2),
		WithRetryDelay(10*time.Millisecond),
		WithSkipSSRF(true),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	seen := rec.seen()
	require.Len(t, seen, 2)
	assert.Equal(t, payload, seen[1])
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestDoRequest_UnreadableBodyFailsBeforeSending(t *testing.T) {
	rec := &bodyRecorder{failFirst: answer503}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, failingReader{})
	require.NoError(t, err)

	resp, err := DoRequest(context.Background(), req, WithRetries(1), WithSkipSSRF(true))
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Empty(t, rec.seen())
}
