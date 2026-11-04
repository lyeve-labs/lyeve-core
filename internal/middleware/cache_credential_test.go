package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// A content read is cached as public only when it carries no credential. A
// session cookie, a bearer token and an API key each reach drafts, closed
// schemas or fields an anonymous reader cannot see, and a shared cache that
// honors public would serve that answer to the next anonymous request. Vary
// does not help with a cookie, and it is not a promise every cache keeps
// with a header, so every credentialed response is private and unstored.
func TestCacheWithPolicy_CredentialedReadsAreServedPrivate(t *testing.T) {
	public := CachePolicy{MaxAge: 60 * time.Second, Public: true}
	serve := CacheWithPolicy(public)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
		{"bearer token", []func(*http.Request){bearer}, true},
		{"API key", []func(*http.Request){key}, true},
		{"an unrelated cookie", []func(*http.Request){session("theme")}, false},
		{"session cookie", []func(*http.Request){session(security.SessionCookieName)}, true},
		{"session cookie on plain HTTP", []func(*http.Request){session(security.SessionCookieNameInsecure)}, true},
		{"session cookie beside a key", []func(*http.Request){session(security.SessionCookieName), key}, true},
		{"session cookie behind a bearer token", []func(*http.Request){session(security.SessionCookieName), bearer}, true},
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

// A credentialed response keeps what the browser may do with it and loses
// what only a shared cache reads. One that already forbids storage, or says
// nothing at all, ends up unstored.
func TestPrivateCacheControlFor_DropsSharedCacheDirectives(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, "private, no-store"},
		{[]string{"public, max-age=300"}, "private, max-age=300"},
		{[]string{"Public, S-MaxAge=600, max-age=60, stale-while-revalidate=600"}, "private, max-age=60, stale-while-revalidate=600"},
		{[]string{"max-age=300, proxy-revalidate"}, "private, max-age=300"},
		{[]string{"public", "max-age=300"}, "private, max-age=300"},
		{[]string{"private, max-age=30"}, "private, max-age=30"},
		{[]string{"public"}, "private"},
		{[]string{"no-store"}, "no-store"},
	} {
		if got := privateCacheControlFor(tc.in); got != tc.want {
			t.Errorf("privateCacheControlFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
