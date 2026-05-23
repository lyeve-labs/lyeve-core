package middleware

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A middleware that wraps the ResponseWriter and forgets Hijack silently breaks
// every WebSocket and SSE handler mounted below it: gorilla asks the writer for
// the socket, does not get it, and the handshake fails with the response already
// committed as a bare 200. The browser reports only a generic error event, so
// the cause sits one layer away from every symptom.
//
// Every wrapper in a chain has to pass the upgrade through.

type hijackableRecorder struct {
	*httptest.ResponseRecorder
	called bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.called = true
	client, server := net.Pipe()
	_ = server.Close()
	return client, bufio.NewReadWriter(bufio.NewReader(&bytes.Buffer{}), bufio.NewWriter(&bytes.Buffer{})), nil
}

func wrappers(inner http.ResponseWriter) map[string]http.ResponseWriter {
	return map[string]http.ResponseWriter{
		"captureResponseWriter": &captureResponseWriter{ResponseWriter: inner},
		"statusRecorder":        &statusRecorder{ResponseWriter: inner},
		"etagWriter":            &etagWriter{ResponseWriter: inner},
		"conditionalWriter":     &conditionalWriter{ResponseWriter: inner},
		"levelCompressWriter":   &levelCompressWriter{ResponseWriter: inner},
	}
}

func TestResponseWriterWrappers_PassTheUpgradeThrough(t *testing.T) {
	for name, w := range wrappers(&hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}) {
		t.Run(name, func(t *testing.T) {
			h, ok := w.(http.Hijacker)
			require.True(t, ok, "%s must implement http.Hijacker or it breaks upgrades below it", name)

			conn, brw, err := h.Hijack()
			require.NoError(t, err)
			assert.NotNil(t, conn)
			assert.NotNil(t, brw)
			_ = conn.Close()
		})
	}
}

// When nothing underneath can be hijacked the wrapper reports it, rather than
// panicking on a bare type assertion and taking the request down with it.
func TestResponseWriterWrappers_ReportAnUnhijackableWriter(t *testing.T) {
	for name, w := range wrappers(httptest.NewRecorder()) {
		t.Run(name, func(t *testing.T) {
			h, ok := w.(http.Hijacker)
			require.True(t, ok)

			var err error
			require.NotPanics(t, func() { _, _, err = h.Hijack() })
			assert.True(t, errors.Is(err, http.ErrNotSupported),
				"%s should return http.ErrNotSupported, got %v", name, err)
		})
	}
}
