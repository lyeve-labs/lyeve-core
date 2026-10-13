package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// A content read is cached as public, and a shared cache keys it on the
// headers Vary names. A bearer token and a key travel in headers Vary names,
// so their responses stay public. A session cookie does not, so a public
// response to a cookie session went to the next request that carried no
// Authorization and no X-API-Key, whoever sent it. A response the cookie may
// have authenticated is private and unstored, and a bearer token in front of
// the cookie is what authenticates instead.
func TestCacheWithPolicy_CookieSessionsAreServedPrivate(t *testing.T) {
	serve := CacheWithPolicy(DefaultContentPolicy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	session := func(name string) func(*http.Request) {
		return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: "token"}) }
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer token") }
	key := func(r *http.Request) { r.Header.Set("X-API-Key", "key") }
	for _, tc := range []struct {
		name    string
		with    []func(*http.Request)
		private bool
	}{
		{"no credential", nil, false},
		{"bearer token", []func(*http.Request){bearer}, false},
		{"API key", []func(*http.Request){key}, false},
		{"an unrelated cookie", []func(*http.Request){session("theme")}, false},
		{"session cookie", []func(*http.Request){session(security.SessionCookieName)}, true},
		{"session cookie on plain HTTP", []func(*http.Request){session(security.SessionCookieNameInsecure)}, true},
		{"session cookie beside a key", []func(*http.Request){session(security.SessionCookieName), key}, true},
		{"session cookie behind a bearer token", []func(*http.Request){session(security.SessionCookieName), bearer}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
			for _, f := range tc.with {
				f(r)
			}
			rr := httptest.NewRecorder()
			serve.ServeHTTP(rr, r)
			cc := rr.Header().Get("Cache-Control")
			if tc.private {
				assert.Equal(t, privateCacheControl, cc)
			} else {
				assert.Contains(t, cc, "public")
			}
			assert.Contains(t, rr.Header().Get("Vary"), "Authorization")
			assert.Contains(t, rr.Header().Get("Vary"), "X-API-Key")
			assert.Empty(t, rr.Header().Get(PrivateResponseHeader))
		})
	}
}
