package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingBody hands out its bytes, then fails the way a dropped connection
// does partway through an upload.
type failingBody struct {
	data []byte
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, errors.New("connection reset by peer")
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *failingBody) Close() error { return nil }

// Request capture reads the body ahead of every plugin middleware, so a read
// error it hid would reach the plugins and the handler as a clean, shorter
// body that no later reader can tell apart from what the client sent. The
// request is answered there instead, and the plugin chain is never entered.
func TestRequestCapture_FailedBodyReadNeverReachesThePluginChain(t *testing.T) {
	for _, rt := range []struct {
		name  string
		path  string
		build func(t *testing.T, opts ...RouterOption) http.Handler
	}{
		{"api", "/api/v1/auth/token", func(t *testing.T, opts ...RouterOption) http.Handler {
			h, err := NewAPIRouter(&fakeDB{engine: "postgres"}, testConfig(), nil, append(opts, WithLifetime(testLifetime(t)))...)
			require.NoError(t, err)
			return h
		}},
		{"admin", "/api/admin/media", func(t *testing.T, opts ...RouterOption) http.Handler {
			h, err := NewAdminRouter(&fakeDB{engine: "postgres"}, testConfig(), append(opts, WithLifetime(testLifetime(t)))...)
			require.NoError(t, err)
			return h
		}},
	} {
		t.Run(rt.name, func(t *testing.T) {
			reached := false
			plugin := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reached = true
					next.ServeHTTP(w, r)
				})
			}
			h := rt.build(t, WithRequestCapture(&memorySink{}), WithMiddleware(plugin))

			req := httptest.NewRequest(http.MethodPost, rt.path, nil)
			req.Body = &failingBody{data: make([]byte, 100)}
			req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.False(t, reached, "a plugin middleware was served a body that failed to read")
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}
