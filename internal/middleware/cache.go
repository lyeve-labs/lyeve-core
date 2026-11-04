// CacheHeaders emits
// Cache-Control, ETag (SHA-256 of response body), and Last-Modified (from
// X-Updated-At) headers. Conditional checks If-None-Match / If-Modified-Since
// and returns 304 when the client's cached copy is still valid. CacheWithPolicy
// combines both. Non-2xx responses are never cached. Place AFTER
// SecurityHeaders so Cache-Control overrides the default no-store.
//
// Usage:
//
//	r.With(middleware.cacheHeaders(middleware.CachePolicy{...})).Get(...)
//	r.With(middleware.conditional()).Get(...)
//	r.With(middleware.CacheWithPolicy(middleware.CachePolicy{...})).Get(...)

package middleware

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/security"
)

// CachePolicy defines the caching behavior for a route group.
type CachePolicy struct {
	// MaxAge is the duration a response is considered fresh in the browser cache.
	// Set to 0 to omit the max-age directive.
	MaxAge time.Duration

	// SharedMaxAge is how long a shared cache (CDN, proxy) may keep the
	// response, in place of MaxAge. Only emitted when Public and MaxAge > 0.
	// Set to 0 to omit.
	SharedMaxAge time.Duration

	// StaleWhileRevalidate allows the browser to use a stale response for up to
	// this duration while a background revalidation happens. Only emitted when
	// MaxAge > 0. Set to 0 to omit.
	StaleWhileRevalidate time.Duration

	// StaleIfError allows the browser to use a stale response when the server
	// returns a 5xx error. Only emitted when MaxAge > 0. Set to 0 to omit.
	StaleIfError time.Duration

	// MustRevalidate forces the browser to revalidate with the server before
	// using a stale response. Set to true for APIs where stale data must not
	// be served beyond max-age (e.g. inventory counts).
	MustRevalidate bool

	// NoCache forces the browser to revalidate with the server before every use.
	// Unlike no-store, the response can still be stored: it just must be
	// revalidated every time. Useful for authenticated dashboards.
	NoCache bool

	// NoStore prevents the response from being stored in any cache.
	NoStore bool

	// NoTransform disables intermediate proxy transformations (image compression,
	// format conversion, etc.) that could break API responses.
	NoTransform bool

	// Public makes the response cacheable by shared caches (CDN, proxy).
	// When false, only the browser may cache the response (Cache-Control: private).
	Public bool
}

// DefaultContentPolicy is the policy the credentialed content reads name. It
// is private because every such read carries a credential, and a request
// that carries one is answered "private, no-store" whatever the policy says,
// so its lifetimes are not sent on those routes. Content meant for a shared
// cache is served on the public delivery routes.
var DefaultContentPolicy = CachePolicy{
	MaxAge:               60 * time.Second,
	StaleWhileRevalidate: 600 * time.Second,
	StaleIfError:         86400 * time.Second,
}

// DefaultDashboardPolicy is the recommended policy for admin dashboard queries:
// 30s fresh, 300s stale-while-revalidate.
var DefaultDashboardPolicy = CachePolicy{
	MaxAge:               30 * time.Second,
	StaleWhileRevalidate: 300 * time.Second,
	NoTransform:          true,
}

// DefaultSchemaPolicy is for rarely-changed schema definitions. It is
// private: the catalog lists only what the caller may read, so two callers
// get different answers, and a session cookie is not in Vary, so a shared
// cache would hand one caller's list to the next.
var DefaultSchemaPolicy = CachePolicy{
	MaxAge:               300 * time.Second,
	StaleWhileRevalidate: 3600 * time.Second,
}

// ImmutablePolicy is for immutable resources such as hashed assets and versioned APIs.
var ImmutablePolicy = CachePolicy{
	MaxAge: 31536000 * time.Second, // 1 year
	Public: true,
}

// NoCachePolicy disables all caching. Use for auth and mutation endpoints.
var NoCachePolicy = CachePolicy{
	NoStore: true,
	NoCache: true,
}

// buildCacheControl assembles the Cache-Control header value from the policy.
// Returns an empty string when no directives are set (skips the header).
func (p CachePolicy) buildCacheControl() string {
	if p.NoStore {
		// no-store implies no-cache. Emitting both is redundant.
		parts := []string{"no-store"}
		if p.NoTransform {
			parts = append(parts, "no-transform")
		}
		return strings.Join(parts, ", ")
	}

	var dirs []string

	// Public or private scoping.
	if p.Public {
		dirs = append(dirs, "public")
	} else {
		dirs = append(dirs, "private")
	}

	// Freshness directives.
	if p.NoCache {
		dirs = append(dirs, "no-cache")
	} else if p.MaxAge > 0 {
		dirs = append(dirs, fmt.Sprintf("max-age=%d", int(p.MaxAge.Seconds())))
		if p.Public && p.SharedMaxAge > 0 {
			dirs = append(dirs, fmt.Sprintf("s-maxage=%d", int(p.SharedMaxAge.Seconds())))
		}
		if p.StaleWhileRevalidate > 0 {
			dirs = append(dirs, fmt.Sprintf("stale-while-revalidate=%d", int(p.StaleWhileRevalidate.Seconds())))
		}
		if p.StaleIfError > 0 {
			dirs = append(dirs, fmt.Sprintf("stale-if-error=%d", int(p.StaleIfError.Seconds())))
		}
	}

	if p.MustRevalidate {
		dirs = append(dirs, "must-revalidate")
	}

	if p.NoTransform {
		dirs = append(dirs, "no-transform")
	}

	return strings.Join(dirs, ", ")
}

// etagWriter wraps http.ResponseWriter to capture the response body so an
// ETag can be computed from the content hash after the handler finishes.
// Implements http.Flusher, http.Pusher, and http.Hijacker so wrapped handlers
// that depend on these interfaces still work.
type etagWriter struct {
	http.ResponseWriter
	buf        bytes.Buffer
	statusCode int
	written    bool
}

// WriteHeader delays flushing until the body is captured, flushing immediately
// for non-2xx responses.
func (ew *etagWriter) WriteHeader(statusCode int) {
	if ew.written {
		return
	}
	ew.statusCode = statusCode
	// Delay WriteHeader until the body is captured and ETag computed.
	// Non-2xx flushes immediately without ETag.
	if statusCode < 200 || statusCode >= 300 {
		ew.ResponseWriter.Header().Del(PrivateResponseHeader)
		ew.ResponseWriter.WriteHeader(statusCode)
		ew.written = true
	}
}

// Write buffers the body when not yet flushed, or passes through to the
// underlying writer after headers are sent.
func (ew *etagWriter) Write(b []byte) (int, error) {
	if ew.written {
		return ew.ResponseWriter.Write(b)
	}
	return ew.buf.Write(b)
}

// Flush switches the writer out of buffering into pass-through streaming. A
// handler that flushes is streaming (SSE, chunked transfer), and an ETag needs
// the complete body a stream never produces, so on the first flush we commit
// the status and any buffered bytes to the client and stream straight through
// on subsequent writes. Without this, buffered SSE frames never reach the
// client and the connection hangs until timeout.
func (ew *etagWriter) Flush() {
	if !ew.written {
		code := ew.statusCode
		if code == 0 {
			code = http.StatusOK
		}
		ew.ResponseWriter.WriteHeader(code)
		ew.written = true
		if ew.buf.Len() > 0 {
			_, _ = ew.ResponseWriter.Write(ew.buf.Bytes())
			ew.buf.Reset()
		}
	}
	if f, ok := ew.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the writer underneath so http.ResponseController can reach
// the connection. Without it the controller stops here and every deadline call
// on a wrapped handler answers ErrNotSupported, which a streaming handler sees
// as "cannot extend my write deadline" and cannot work around.
func (ew *etagWriter) Unwrap() http.ResponseWriter {
	return ew.ResponseWriter
}

// Push delegates to the underlying Pusher if supported.
func (ew *etagWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := ew.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Hijack delegates to the underlying Hijacker if supported.
func (ew *etagWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := ew.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("middleware: underlying ResponseWriter does not support hijacking: %w", http.ErrNotSupported)
}

// PrivateResponseHeader is the internal signal a handler sets when the
// response it wrote belongs to the caller alone, such as one that can carry
// unpublished content. The cache middleware strips it and serves the response
// private and unstored, whatever the route's policy says.
const PrivateResponseHeader = "X-Cache-Private"

// privateCacheControl is what a response marked private is served with.
const privateCacheControl = "private, no-store"

// CacheHeaders wraps a handler to emit Cache-Control + ETag + Last-Modified
// headers based on the given policy. ETags are strong validators computed from
// the SHA-256 hash of the response body. Last-Modified is derived from the
// X-Updated-At header the handler sets.
//
// Apply AFTER SecurityHeaders so the Cache-Control header overrides the
// SecurityHeaders default of no-store.
func cacheHeaders(policy CachePolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ew := &etagWriter{ResponseWriter: w}

			next.ServeHTTP(ew, r)

			// Non-2xx: headers already flushed.
			if ew.written {
				return
			}

			// Compute ETag from body hash.
			body := ew.buf.Bytes()
			if len(body) > 0 {
				hash := sha256.Sum256(body)
				ew.Header().Set("ETag", `"`+hex.EncodeToString(hash[:])+`"`)
			}

			if ew.Header().Get(PrivateResponseHeader) != "" || CarriesCredential(r) {
				ew.Header().Del(PrivateResponseHeader)
				ew.Header().Set("Cache-Control", privateCacheControl)
			} else if cc := policy.buildCacheControl(); cc != "" {
				ew.Header().Set("Cache-Control", cc)
			}

			// Last-Modified from X-Updated-At, an internal signal set by content handlers.
			if updatedAt := ew.Header().Get("X-Updated-At"); updatedAt != "" {
				if t, err := time.Parse(time.RFC3339, updatedAt); err == nil {
					ew.Header().Set("Last-Modified", t.UTC().Format(http.TimeFormat))
				}
			}

			// Vary: Authorization prevents CDNs from serving cached authenticated
			// responses to unauthenticated callers. X-API-Key does the same for
			// keys, which answer differently from one another because each is
			// held to its own schema list, scopes and roles. The handler and
			// the middleware before it may each have added their own value
			// (Origin from CORS, Accept-Language from a localized read), so every
			// value is merged rather than only the first one Get would return.
			ew.Header().Set("Vary", mergeVary(ew.Header().Values("Vary"), "Authorization", "X-API-Key"))

			if ew.statusCode == 0 {
				ew.statusCode = http.StatusOK
			}
			ew.ResponseWriter.WriteHeader(ew.statusCode)
			ew.written = true
			_, _ = ew.ResponseWriter.Write(body) // err suppressed: response write to client
		})
	}
}

// CarriesCredential reports whether the request presents a credential: an
// Authorization header, an API key, or a session cookie. A response computed
// for any of them may hold what an anonymous reader cannot see, so it is
// theirs alone. Vary names the two headers, but a shared cache that honors
// public is not bound to key on them, and Vary cannot name one cookie among
// the others a browser sends. A credential that fails to authenticate only
// costs the response its shared caching. The public delivery routes refuse a
// request this reports true for, so a credential means one thing on both
// paths.
func CarriesCredential(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" {
		return true
	}
	for _, name := range []string{security.SessionCookieName, security.SessionCookieNameInsecure} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return true
		}
	}
	return false
}

// CredentialedPrivate keeps every response to a request that carries a
// credential out of shared caches, whatever the route set. A credential's
// tenant wins over the host, so the answer can belong to another tenant than
// the URL names, and a CDN keyed on host and URL would serve it, or its 404,
// to that host's visitors. A Cache-Control that a shared cache may honor
// becomes private and keeps its max-age for the browser, one with no
// Cache-Control becomes private and unstored, and the CDN-only headers go.
// A request with no credential passes through untouched.
func CredentialedPrivate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !CarriesCredential(r) {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&privateWriter{ResponseWriter: w}, r)
	})
}

// cdnOnlyHeaders are read by a CDN in place of Cache-Control.
var cdnOnlyHeaders = []string{"CDN-Cache-Control", "Cloudflare-CDN-Cache-Control", "Surrogate-Control"}

// sharedOnlyDirectives grant or tune storage in a shared cache alone.
var sharedOnlyDirectives = map[string]bool{"public": true, "s-maxage": true, "proxy-revalidate": true}

// privateCacheControlFor rewrites a response's Cache-Control so no shared
// cache stores it. A bare max-age counts as shared too: a shared cache may
// store it for a request whose credential is a cookie or a key.
func privateCacheControlFor(values []string) string {
	var kept []string
	seen := false
	for _, v := range values {
		for _, d := range strings.Split(v, ",") {
			d = strings.TrimSpace(d)
			name := strings.ToLower(strings.TrimSpace(strings.SplitN(d, "=", 2)[0]))
			seen = seen || name != ""
			switch {
			case name == "":
			case name == "no-store":
				return strings.Join(values, ", ")
			case name == "private" || sharedOnlyDirectives[name]:
			default:
				kept = append(kept, d)
			}
		}
	}
	if !seen {
		return privateCacheControl
	}
	return strings.Join(append([]string{"private"}, kept...), ", ")
}

// privateWriter applies CredentialedPrivate's rewrite when the status line
// goes out, after every handler and middleware below it set its headers.
type privateWriter struct {
	http.ResponseWriter
	done bool
}

func (w *privateWriter) WriteHeader(code int) {
	if !w.done && code >= http.StatusOK {
		w.done = true
		h := w.Header()
		h.Set("Cache-Control", privateCacheControlFor(h.Values("Cache-Control")))
		for _, name := range cdnOnlyHeaders {
			h.Del(name)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *privateWriter) Write(b []byte) (int, error) {
	if !w.done {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush sends the rewritten headers first when nothing was written yet,
// because a flush commits them as they stand.
func (w *privateWriter) Flush() {
	if !w.done {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands the connection over unchanged: a WebSocket made with a
// token writes its own handshake and carries no cache headers.
func (w *privateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("middleware: underlying ResponseWriter does not support hijacking: %w", http.ErrNotSupported)
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (w *privateWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// CacheWithPolicy combines CacheHeaders + Conditional in one middleware.
// CacheHeaders runs first (inner) to compute ETag / Last-Modified from the
// handler response, then Conditional (outer) checks If-None-Match / If-Modified-Since
// and returns 304 when the content hasn't changed.
func CacheWithPolicy(policy CachePolicy) func(http.Handler) http.Handler {
	headers := cacheHeaders(policy)
	cond := conditional()
	return func(next http.Handler) http.Handler {
		return cond(headers(next))
	}
}

// Conditional inspects If-None-Match and If-Modified-Since and returns 304
// when the client's cached copy is still valid. The handler still runs. The
// middleware wraps the response writer to compare the computed ETag or
// Last-Modified against the client's conditional headers and discards the
// body on match. Requires an ETag middleware (CacheHeaders) in the chain.
func conditional() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ifNoneMatch := r.Header.Get("If-None-Match")
			ifModSince := r.Header.Get("If-Modified-Since")

			if ifNoneMatch == "" && ifModSince == "" {
				next.ServeHTTP(w, r)
				return
			}

			cw := &conditionalWriter{
				ResponseWriter: w,
				ifNoneMatch:    ifNoneMatch,
				ifModSince:     ifModSince,
			}
			next.ServeHTTP(cw, r)

			// Headers already written (non-2xx, streaming): pass through.
			if cw.written {
				return
			}

			statusCode := cw.statusCode
			if statusCode == 0 {
				statusCode = http.StatusOK
			}

			// Only 2xx responses are 304-eligible.
			if statusCode >= 200 && statusCode < 300 {
				if notModified(cw.Header(), ifNoneMatch, ifModSince) {
					h := w.Header()
					delHeaders := []string{"Content-Type", "Content-Length", "Content-Encoding"}
					for _, k := range delHeaders {
						h.Del(k)
					}
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}

			w.WriteHeader(statusCode)
			cw.written = true
			if cw.buf.Len() > 0 {
				_, _ = w.Write(cw.buf.Bytes()) // err suppressed: response write to client
			}
		})
	}
}

// conditionalWriter buffers the response for ETag/Last-Modified comparison
// against the client's If-None-Match / If-Modified-Since before flushing.
type conditionalWriter struct {
	http.ResponseWriter
	buf         bytes.Buffer
	statusCode  int
	written     bool
	ifNoneMatch string
	ifModSince  string
}

// WriteHeader captures the status code, flushing immediately for non-2xx
// responses so the body is not buffered.
func (cw *conditionalWriter) WriteHeader(statusCode int) {
	if cw.written {
		return
	}
	cw.statusCode = statusCode
	if statusCode < 200 || statusCode >= 300 {
		cw.ResponseWriter.WriteHeader(statusCode)
		cw.written = true
	}
}

// Write buffers the body when not yet flushed, or passes through after
// headers are sent.
func (cw *conditionalWriter) Write(b []byte) (int, error) {
	if cw.written {
		return cw.ResponseWriter.Write(b)
	}
	return cw.buf.Write(b)
}

// Header returns the response headers.
func (cw *conditionalWriter) Header() http.Header {
	return cw.ResponseWriter.Header()
}

// Flush delegates to the underlying Flusher if supported.
func (cw *conditionalWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the writer underneath so http.ResponseController can reach
// the connection, for the same reason etagWriter does.
func (cw *conditionalWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

// Push delegates to the underlying Pusher if supported.
func (cw *conditionalWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := cw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Hijack delegates to the underlying Hijacker if supported.
func (cw *conditionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := cw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("middleware: underlying ResponseWriter does not support hijacking: %w", http.ErrNotSupported)
}

// notModified returns true when the response's ETag or Last-Modified match
// the client's conditional headers, indicating a 304 response.
func notModified(h http.Header, ifNoneMatch, ifModSince string) bool {
	etag := strings.TrimSpace(h.Get("ETag"))

	// If-None-Match takes precedence over If-Modified-Since per RFC 7232 §3.3.
	if ifNoneMatch != "" && etag != "" {
		return etagMatch(etag, ifNoneMatch)
	}

	if ifModSince != "" {
		lm := h.Get("Last-Modified")
		if lm == "" {
			return false
		}
		tReq, err := time.Parse(http.TimeFormat, ifModSince)
		if err != nil {
			return false
		}
		tResp, err := time.Parse(http.TimeFormat, lm)
		if err != nil {
			return false
		}
		return !tResp.After(tReq)
	}

	return false
}

// etagMatch performs RFC 7232 §2.3.2 weak comparison. A weak ETag matches
// any ETag with the same opaque-tag. The "*" wildcard matches any ETag.
func etagMatch(etag, ifNoneMatch string) bool {
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}

	// Strip quotes and W/ prefix to get the opaque-tag.
	ourValue := strings.TrimPrefix(etag, `W/`)
	ourValue = strings.Trim(ourValue, `"`)

	etags := strings.Split(ifNoneMatch, ",")
	for _, e := range etags {
		e = strings.TrimSpace(e)
		if e == "*" {
			return true
		}
		e = strings.TrimPrefix(e, `W/`)
		e = strings.Trim(e, `"`)
		if e == ourValue {
			return true
		}
	}
	return false
}

// mergeVary folds every Vary value, each possibly a comma-separated list, and
// the extra members into one list with no duplicates, keeping first-seen order.
// Members compare case-insensitively, as header names do.
func mergeVary(values []string, extra ...string) string {
	seen := make(map[string]struct{})
	var members []string
	add := func(raw string) {
		for _, m := range strings.Split(raw, ",") {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			key := strings.ToLower(m)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			members = append(members, m)
		}
	}
	for _, v := range values {
		add(v)
	}
	for _, e := range extra {
		add(e)
	}
	return strings.Join(members, ", ")
}
