package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

const loopbackTestBurst = 3

// loopbackTestServer serves the built-in stack with a limiter that refills
// too slowly to matter inside a test, ahead of a handler that records whether
// the loopback header reached it.
func loopbackTestServer(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	sawMark := new(bool)
	r := chi.NewRouter()
	applyBuiltinMiddleware(r, &config.Config{MaxBodyBytes: 1 << 20, RateLimitRPS: 0.001, RateLimitBurst: loopbackTestBurst}, nil,
		apimw.ClientAddress(nil))
	r.Get("/api/v1/me", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get(apimw.LoopbackHeader) != "" {
			*sawMark = true
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, sawMark
}

// callAs sends one request as tenant with token, marked by mark.
func callAs(t *testing.T, srv *httptest.Server, tenant, token string, mark func(*http.Request)) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/me", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tenant-ID", tenant)
	mark(req)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestLoopbackRateLimit_OneTenantFloodLeavesAnotherServed(t *testing.T) {
	srv, sawMark := loopbackTestServer(t)

	for i := 0; i < loopbackTestBurst; i++ {
		require.Equal(t, http.StatusOK, callAs(t, srv, "acme", "token-acme", apimw.MarkLoopback), "call %d", i+1)
	}
	require.Equal(t, http.StatusTooManyRequests, callAs(t, srv, "acme", "token-acme", apimw.MarkLoopback),
		"the flooding tenant must reach its own limit")

	assert.Equal(t, http.StatusOK, callAs(t, srv, "globex", "token-globex", apimw.MarkLoopback),
		"another tenant's loopback call must not count against the flooding tenant's bucket")
	assert.False(t, *sawMark, "the mark must be removed before the handler")
}

func TestLoopbackRateLimit_ForgedMarkKeepsAddressBucket(t *testing.T) {
	srv, sawMark := loopbackTestServer(t)
	forge := func(req *http.Request) { req.Header.Set(apimw.LoopbackHeader, "not-the-process-mark") }

	for i := 0; i < loopbackTestBurst; i++ {
		require.Equal(t, http.StatusOK, callAs(t, srv, "acme", "token-acme", forge), "call %d", i+1)
	}
	require.Equal(t, http.StatusTooManyRequests, callAs(t, srv, "acme", "token-acme", forge))

	assert.Equal(t, http.StatusTooManyRequests, callAs(t, srv, "globex", "token-globex", forge),
		"a forged mark must not let a caller pick its own bucket")
	assert.False(t, *sawMark, "a forged mark must be removed before the handler")
}
