package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// A browser signed in to the console over TLS holds __Host-sys_session, and
// its own calls to the engine (media upload, the log stream) carry only that.
// The engine has to authenticate them, and on a TLS deployment it must not
// take the plain name, which any host on the parent domain could set.
func TestJWTAuth_SessionCookieNameFollowsSecureSetting(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	tok := makeJWT(secret, uuid.New(), "cookie@test.com", []string{"admin"}, 3600)

	for _, tc := range []struct {
		name   string
		secure bool
		cookie string
		authed bool
	}{
		{"tls_reads_prefixed_name", true, security.SessionCookieName, true},
		{"tls_ignores_plain_name", true, security.SessionCookieNameInsecure, false},
		{"plaintext_reads_plain_name", false, security.SessionCookieNameInsecure, true},
		{"plaintext_still_reads_prefixed_name", false, security.SessionCookieName, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/api/admin/media", nil)
			r.AddCookie(&http.Cookie{Name: tc.cookie, Value: tok})

			var authed bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authed = claimsFromCtx(r) != nil
			})
			jwtAuth([]string{secret}, tc.secure)(next).ServeHTTP(httptest.NewRecorder(), r)
			assert.Equal(t, tc.authed, authed, "cookie %q with secure=%v", tc.cookie, tc.secure)
		})
	}
}

// Over plain HTTP a browser may still hold a prefixed cookie from an earlier
// sign-in. The plain name is the one this deployment writes, so it wins.
func TestJWTAuth_PlaintextPrefersPlainSessionCookie(t *testing.T) {
	t.Parallel()

	secret := "test-secret-at-least-32-bytes-long!"
	current := makeJWT(secret, uuid.New(), "current@test.com", []string{"admin"}, 3600)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: security.SessionCookieName, Value: "stale-and-unparseable"})
	r.AddCookie(&http.Cookie{Name: security.SessionCookieNameInsecure, Value: current})

	var email string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := claimsFromCtx(r); c != nil {
			email = c.Email
		}
	})
	jwtAuth([]string{secret}, false)(next).ServeHTTP(httptest.NewRecorder(), r)
	assert.Equal(t, "current@test.com", email)
}
