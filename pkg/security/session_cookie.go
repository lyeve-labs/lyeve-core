package security

// SessionCookieName is the session cookie's name on a deployment that serves
// over TLS. The __Host- prefix makes the browser refuse the cookie unless it is
// Secure, host-only and scoped to /, so neither a sibling subdomain nor a
// plain-HTTP response can plant or replace a session.
const SessionCookieName = "__Host-sys_session"

// SessionCookieNameInsecure is the name over plain HTTP. A browser discards a
// __Host- cookie it cannot also mark Secure, so a local server that wrote the
// prefixed name would sign nobody in.
const SessionCookieNameInsecure = "sys_session"

// SessionCookieNameFor returns the name the session cookie is written under.
// secure is the deployment's secure-cookie setting.
func SessionCookieNameFor(secure bool) string {
	if secure {
		return SessionCookieName
	}
	return SessionCookieNameInsecure
}

// SessionCookieNamesFor returns the names a session is read from, in order.
//
// Over TLS only the prefixed name counts. Accepting the plain name as well would
// let any host able to set a cookie on the parent domain choose the session the
// browser presents, which is the one thing the prefix exists to stop. Over plain
// HTTP the prefix guarantees nothing, so the plain name is read first and the
// prefixed one still works, since a browser keeps a __Host- cookie on a
// loopback origin.
func SessionCookieNamesFor(secure bool) []string {
	if secure {
		return []string{SessionCookieName}
	}
	return []string{SessionCookieNameInsecure, SessionCookieName}
}
