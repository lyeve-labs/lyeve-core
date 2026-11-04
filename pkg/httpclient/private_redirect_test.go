package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A request with the address check skipped may reach a private address on
// purpose, and its target was the one an operator allowed. A redirect names
// a target nobody allowed, so it is handed back to the caller unfollowed.
func TestDoRequest_SkippedCheckDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			reached.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start", nil)
	require.NoError(t, err)
	resp, err := DoRequest(context.Background(), req, WithSkipSSRF(true), WithRetries(0))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, int32(0), reached.Load(), "the redirect target was requested")
}
