package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// assertErrorEnvelope holds a refusal to the answer every other refusal of the
// engine gives: labeled JSON, so a client that reads the type before the body
// parses it, carrying the message, the status's code and the request id that
// X-Request-ID names, and nothing else.
func assertErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, status int, message, code string) {
	t.Helper()
	assert.Equal(t, status, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body = %q", rec.Body.String())
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"code", "error", "request_id"}, keys)
	assert.Equal(t, message, body["error"])
	assert.Equal(t, code, body["code"])

	id := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, id, "the correlation middleware names the request")
	assert.Equal(t, id, body["request_id"])
}

// guarded runs h behind the correlation middleware, as both routers do, so a
// refusal can carry the request id.
func guarded(h http.Handler) http.Handler {
	return CorrelationID()(h)
}

func refuseIfReached(t *testing.T) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the guard let the request through")
	})
}

func TestRequireScoped_RefusalIsTheErrorEnvelope(t *testing.T) {
	h := guarded(RequireScoped(refuseIfReached(t)))

	claims := &core.AuthClaims{
		UserID:   "key-without-delete",
		Roles:    []string{"viewer"},
		Scopes:   []string{"content:read"},
		IsAPIKey: true,
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/content/posts/123", nil)
	req = req.WithContext(context.WithValue(req.Context(), core.ClaimsKey, claims))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assertErrorEnvelope(t, rec, http.StatusForbidden, "insufficient scope", "forbidden")
}

func TestIPAllowlist_RefusalIsTheErrorEnvelope(t *testing.T) {
	allow, err := IPAllowlist([]string{"10.0.0.0/8"})
	require.NoError(t, err)
	h := guarded(allow(refuseIfReached(t)))

	for name, remote := range map[string]string{
		"address outside the list":    "203.0.113.9:4000",
		"address that does not parse": "not-an-address",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
			req.RemoteAddr = remote
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assertErrorEnvelope(t, rec, http.StatusForbidden, "forbidden", "forbidden")
		})
	}
}

func TestAllowedHosts_RefusalIsTheErrorEnvelope(t *testing.T) {
	h := guarded(AllowedHosts([]string{"cms.example.com"})(refuseIfReached(t)))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// The helper names the statuses a client meets every day and gives any
	// other its number, so a 421 carries "http_421".
	assertErrorEnvelope(t, rec, http.StatusMisdirectedRequest, "misdirected request", "http_421")
}
