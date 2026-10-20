package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A write after the server's write timeout reaches the client only when the
// handler moved the deadline first. The control writes the same body
// without moving it and loses it.
func TestStreamDeadline_HoldsTheConnectionPastTheWriteTimeout(t *testing.T) {
	t.Parallel()

	const writeTimeout = 500 * time.Millisecond
	serve := func(hold bool) (string, error) {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d := NewStreamDeadline(w, 4*writeTimeout, nil)
			if hold && !d.Hold() {
				return
			}
			_, _ = fmt.Fprint(w, "first\n")
			_ = http.NewResponseController(w).Flush()
			time.Sleep(writeTimeout + 300*time.Millisecond)
			if hold && !d.Hold() {
				return
			}
			_, _ = fmt.Fprint(w, "after the deadline\n")
		}))
		srv.Config.WriteTimeout = writeTimeout
		srv.Start()
		defer srv.Close()

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}

	body, err := serve(true)
	require.NoError(t, err)
	assert.Equal(t, "first\nafter the deadline\n", body)

	body, err = serve(false)
	assert.NotContains(t, body, "after the deadline", "the control must lose its late write, err %v", err)
}

// A writer chain that cannot reach the connection is reported once and the
// stream goes on, ending at the server's deadline as it did before.
func TestStreamDeadline_ReportsAnUnsupportedChainOnce(t *testing.T) {
	t.Parallel()

	var reported []error
	d := NewStreamDeadline(httptest.NewRecorder(), time.Second, func(err error) { reported = append(reported, err) })
	assert.True(t, d.Hold())
	assert.True(t, d.Hold())
	require.Len(t, reported, 1)
	assert.ErrorIs(t, reported[0], http.ErrNotSupported)

	assert.True(t, NewStreamDeadline(httptest.NewRecorder(), time.Second, nil).Hold(),
		"no callback is the caller's choice, not a refusal")
}
