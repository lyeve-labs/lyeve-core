package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every credential header a request carries is in its capture, with the
// value replaced. A header missing from the capture reads as one the client
// never sent. The request is refused for its fake credentials, as a probe's
// is, and is captured all the same.
func TestRequestCapture_KeepsCredentialHeadersRedacted(t *testing.T) {
	for _, rt := range []struct {
		name  string
		path  string
		build func(t *testing.T, opts ...RouterOption) http.Handler
	}{
		{"api", "/api/v1/health", func(t *testing.T, opts ...RouterOption) http.Handler {
			h, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, append(opts, WithLifetime(testLifetime(t)))...)
			require.NoError(t, err)
			return h
		}},
		{"admin", "/api/admin/health", func(t *testing.T, opts ...RouterOption) http.Handler {
			h, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), append(opts, WithLifetime(testLifetime(t)))...)
			require.NoError(t, err)
			return h
		}},
	} {
		t.Run(rt.name, func(t *testing.T) {
			sink := &memorySink{}
			h := rt.build(t, WithRequestCapture(sink))
			req := httptest.NewRequest(http.MethodGet, rt.path, nil)
			req.Header.Set("Authorization", "Bearer not-a-token")
			req.Header.Set("Cookie", "session=s; other=s")
			req.Header.Set("X-Api-Key", "not-a-key")
			h.ServeHTTP(httptest.NewRecorder(), req)

			sink.mu.Lock()
			defer sink.mu.Unlock()
			require.NotEmpty(t, sink.entries, "nothing was captured")
			got := sink.entries[len(sink.entries)-1].RequestHeaders
			for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
				require.Equal(t, []string{"[REDACTED]"}, got[name], name)
			}
		})
	}
}
