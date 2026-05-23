package security

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSessionCookieNameFor_PrefixOnlyOverTLS(t *testing.T) {
	assert.Equal(t, "__Host-sys_session", SessionCookieNameFor(true))
	assert.Equal(t, "sys_session", SessionCookieNameFor(false))
}

func TestSessionCookieNamesFor_TLSReadsOnlyThePrefixedName(t *testing.T) {
	names := SessionCookieNamesFor(true)
	assert.Equal(t, []string{SessionCookieName}, names)
	for _, n := range names {
		assert.True(t, strings.HasPrefix(n, "__Host-"), "a TLS deployment must not read %q", n)
	}
}

func TestSessionCookieNamesFor_PlainHTTPPrefersThePlainName(t *testing.T) {
	assert.Equal(t, []string{SessionCookieNameInsecure, SessionCookieName}, SessionCookieNamesFor(false))
}
