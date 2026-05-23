package middleware

import (
	"net/http"
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/httpx"
)

// PathSanitize rejects requests whose URL path contains null bytes, control
// characters, or fullwidth Unicode characters that could cause path-confusion
// attacks.
//
// Null bytes (%00) cause truncation-based bypasses in downstream systems.
// Control characters (\x00-\x1f, \x7f) inject escape sequences into terminal
// logs or downstream protocols. Fullwidth characters (U+FF01-U+FF5E) are
// homoglyphs of ASCII printables and may be normalized by some systems,
// allowing schema name bypass. Defense-in-depth: the primary fix is framework
// path normalization. This middleware catches what slips through.
//
// Place early in the chain, before routing.
func PathSanitize() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// r.URL.Path is already decoded by net/http; %00 is the literal \x00 byte.
			path := r.URL.Path
			if ContainsNullOrControl(path) {
				httpx.ErrorReq(w, r, http.StatusBadRequest, "request path contains invalid characters")
				return
			}
			if containsFullwidth(path) {
				httpx.ErrorReq(w, r, http.StatusBadRequest, "request path contains invalid characters")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ContainsNullOrControl reports whether s contains a null byte (0x00),
// a C0 control character (0x01-0x1f), or DEL (0x7f).
func ContainsNullOrControl(s string) bool {
	for _, r := range s {
		if r <= 0x1f || r == 0x7f {
			return true
		}
	}
	return false
}

// containsFullwidth reports whether s contains any fullwidth Unicode
// character in the range U+FF01-U+FF5E. These are the fullwidth
// equivalents of ASCII printable characters (e.g. U+FF21 = 'Ａ' for 'A').
func containsFullwidth(s string) bool {
	for _, r := range s {
		if r >= 0xff01 && r <= 0xff5e {
			return true
		}
	}
	return false
}

// SanitizePathParam checks a URL path parameter for null bytes, control
// characters, and fullwidth homoglyphs. Returns true on invalid input.
// Defense-in-depth for handlers that extract path params into DB queries.
func sanitizePathParam(param string) bool {
	return ContainsNullOrControl(param) || containsFullwidth(param)
}

// FullwidthToASCII maps fullwidth Unicode characters to their ASCII
// equivalents. Used in tests and diagnostics only.
func fullwidthToASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0xff01 && r <= 0xff5e {
			b.WriteRune(r - 0xff01 + '!')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// init validates the fullwidth range at package init time.
