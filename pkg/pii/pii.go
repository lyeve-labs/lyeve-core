// Package pii provides PII masking, redaction, and sanitization utilities:
// IP addresses, email, phone numbers, user agents, error details, stack
// traces, and secrets. All functions are nil-safe and tolerate empty strings.
package pii

import (
	"regexp"
	"slices"
	"strings"
)

// Redacted is the sentinel string substituted for fully redacted values.
const Redacted = "[redacted]"

// MaskIP partially masks an IP address, preserving the last segment.
// IPv4: "***.***.***.42". IPv6: "****:****:...:7334".
// Non-IP input and localhost (127.0.0.1, ::1) are returned unchanged.
func MaskIP(ip string) string {
	if ip == "" {
		return ip
	}
	if ip == "::1" || ip == "127.0.0.1" {
		return ip // localhost is not sensitive
	}
	// Try IPv4.
	if strings.Count(ip, ".") == 3 {
		lastDot := strings.LastIndexByte(ip, '.')
		if lastDot > 0 {
			return "***.***.***" + ip[lastDot:]
		}
	}
	// Try IPv6.
	if strings.Contains(ip, ":") {
		parts := strings.Split(ip, ":")
		if len(parts) >= 2 {
			for i := 0; i < len(parts)-1; i++ {
				parts[i] = "****"
			}
			return strings.Join(parts, ":")
		}
	}
	return ip
}

// MaskEmail replaces the local part with its first character plus "***":
// jane@example.com -> j***@example.com. Empty local parts pass through.
func MaskEmail(email string) string {
	if email == "" || !strings.Contains(email, "@") {
		return email
	}
	parts := strings.SplitN(email, "@", 2)
	local := parts[0]
	domain := parts[1]
	if len(local) == 0 {
		return email
	}
	if len(local) <= 2 {
		return local[:1] + "***@" + domain
	}
	return local[:1] + "***@" + domain
}

// MaskPhone replaces all but the last four digits with "*", preserving
// formatting characters. Example: "555-123-4567" -> "***-***-4567".
func MaskPhone(phone string) string {
	if phone == "" {
		return phone
	}
	// Strip non-digits to find the last 4.
	digits := regexp.MustCompile(`\d`).FindAllString(phone, -1)
	if len(digits) <= 4 {
		return strings.Repeat("*", len(digits))
	}
	// Find position of the 4th-to-last digit in the original string.
	digitCount := 0
	for i := len(phone) - 1; i >= 0; i-- {
		if phone[i] >= '0' && phone[i] <= '9' {
			digitCount++
			if digitCount == 4 {
				// Build masked prefix: keep original formatting but mask digits.
				result := make([]byte, i)
				for j := 0; j < i; j++ {
					if phone[j] >= '0' && phone[j] <= '9' {
						result[j] = '*'
					} else {
						result[j] = phone[j]
					}
				}
				return string(result) + phone[i:]
			}
		}
	}
	return phone
}

// MaskUserAgent strips trailing UA tokens, keeping everything up to and
// including the browser product (e.g. Chrome/120.0.0.0). Strips downstream
// engine tokens. Short or unrecognized UAs pass through.
func MaskUserAgent(ua string) string {
	if ua == "" || len(ua) < 12 {
		return ua
	}
	// Find the browser product token and truncate after it.
	idx := strings.Index(ua, "Chrome/")
	if idx >= 0 {
		return extractProduct(ua, idx)
	}
	idx = strings.Index(ua, "Firefox/")
	if idx >= 0 {
		return extractProduct(ua, idx)
	}
	idx = strings.Index(ua, "Safari/")
	if idx >= 0 && !strings.Contains(ua, "Chrome") {
		return extractProduct(ua, idx)
	}
	// Fallback: return just the first product token.
	space := strings.IndexByte(ua, ' ')
	if space > 0 {
		return ua[:space]
	}
	return ua
}

// extractProduct returns ua[:end] where end is the next space after idx.
func extractProduct(ua string, idx int) string {
	end := idx
	for end < len(ua) && ua[end] != ' ' {
		end++
	}
	return ua[:end]
}

// Sanitize replaces email addresses in s with [email_address].
func Sanitize(s string) string {
	return sanitizeEmailRe.ReplaceAllStringFunc(s, func(m string) string {
		return "[email_address]"
	})
}

var sanitizeEmailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// SanitizeDetail replaces email addresses in error-detail text with
// [email_address]. Empty input returns "".
func SanitizeDetail(detail string) string {
	if detail == "" {
		return detail
	}
	s := sanitizeEmailRe.ReplaceAllString(detail, "[email_address]")
	return s
}

// SanitizeStackTrace redacts absolute file paths (-> basename only), email
// addresses, and quoted function-argument strings (-> [args-redacted]) from
// Go stack traces. Goroutine headers, function names, file:line references,
// and trace structure are preserved. Prevents internal paths and accidental
// secrets from reaching a stored stack trace.
func SanitizeStackTrace(trace string) string {
	if trace == "" {
		return trace
	}
	trace = sanitizeEmailRe.ReplaceAllString(trace, "[email_address]")
	trace = sanitizePathRE.ReplaceAllStringFunc(trace, func(match string) string {
		// file:line with absolute path -> keep filename only.
		if loc := fileLinePathRE.FindStringSubmatch(match); loc != nil {
			return loc[2] + ":" + loc[3]
		}
		slash := strings.LastIndex(match, "/")
		bs := strings.LastIndex(match, "\\")
		if slash >= 0 || bs >= 0 {
			sep := slash
			if bs > sep {
				sep = bs
			}
			return match[sep+1:]
		}
		return match
	})
	// Quoted strings in argument positions may contain secrets/tokens.
	trace = funcArgsRE.ReplaceAllString(trace, "([args-redacted])")
	return trace
}

var (
	// fileLinePathRE captures (prefix, filename, line) from indented
	// file:line references like "\t/app/internal/handler.go:42".
	fileLinePathRE = regexp.MustCompile(`^\s*([/\w.-]+/+)*([\w.-]+\.go):(\d+)`)

	// sanitizePathRE matches Go source file:line references in stack traces.
	sanitizePathRE = regexp.MustCompile(`[\w/.-]+\.go:\d+`)

	// funcArgsRE matches parenthesized argument lists containing quoted
	// strings in Go stack traces (e.g. pkg.Func("secret", 42)).
	funcArgsRE = regexp.MustCompile(`\([^)]*"[^"]*"[^)]*\)`)
)

// IsSuperAdmin reports whether roles contains "super_admin".
func IsSuperAdmin(roles []string) bool {
	return slices.Contains(roles, "super_admin")
}

// Mask applies fn to *s in place. No-op when s is nil or *s is "".
func Mask(s *string, fn func(string) string) {
	if s == nil || *s == "" {
		return
	}
	*s = fn(*s)
}
