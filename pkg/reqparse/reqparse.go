// Package reqparse provides typed HTTP request parameter parsers shared by plugin
// handlers: UUID extraction, integer query params, client IP, and pagination.
// All parsing errors are typed so httpx.StatusFor can map them to HTTP 400.
//
// Prefer it over the deprecated uuidx package.
package reqparse

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Error types

// InvalidUUIDError is returned by ParseUUID when a chi URL parameter is missing
// or not a valid UUID. httpx.StatusFor maps it to 400 Bad Request.
type InvalidUUIDError struct {
	Param string
	Raw   string
	Err   error
}

// Error returns a human-readable description of the UUID parse failure.
func (e *InvalidUUIDError) Error() string {
	if e.Raw == "" {
		return fmt.Sprintf("reqparse: missing URL parameter %q", e.Param)
	}
	if e.Err != nil {
		return fmt.Sprintf("reqparse: invalid UUID for parameter %q: %s: %v", e.Param, e.Raw, e.Err)
	}
	return fmt.Sprintf("reqparse: invalid UUID for parameter %q: %s", e.Param, e.Raw)
}

// Unwrap returns the underlying error, if any.
func (e *InvalidUUIDError) Unwrap() error { return e.Err }

// MissingParamError is returned when a required request parameter is absent.
// httpx.StatusFor maps it to 400 Bad Request.
type MissingParamError struct {
	Param string // query param name or URL param name
}

// Error returns a human-readable description of the missing parameter.
func (e *MissingParamError) Error() string {
	return fmt.Sprintf("reqparse: missing required parameter %q", e.Param)
}

// InvalidQueryIntError is returned by QueryInt when a query parameter is not a
// valid integer. httpx.StatusFor maps it to 400 Bad Request.
type InvalidQueryIntError struct {
	Key string
	Raw string
	Err error
}

// Error returns a human-readable description of the invalid integer query parameter.
func (e *InvalidQueryIntError) Error() string {
	return fmt.Sprintf("reqparse: invalid integer for query parameter %q: %s", e.Key, e.Raw)
}

// Unwrap returns the underlying parse error.
func (e *InvalidQueryIntError) Unwrap() error { return e.Err }

// InvalidPaginationError is returned by ParsePagination when offset or limit
// query parameters are invalid (negative values, non-integer values).
// httpx.StatusFor maps it to 400 Bad Request.
type InvalidPaginationError struct {
	Field string // "offset" or "limit"
	Raw   string
}

// Error returns a human-readable description of the invalid pagination parameter.
func (e *InvalidPaginationError) Error() string {
	return fmt.Sprintf("reqparse: invalid pagination parameter %q: %s", e.Field, e.Raw)
}

// Parsers

// ParseUUID extracts a URL path parameter by name and parses it as a UUID.
// It tries chi.URLParam first (for plugins mounted via chi routes), then
// falls back to r.PathValue (for stdlib-based routing). Returns
// InvalidUUIDError on failure.
//
// Usage:
//
//	id, err := reqparse.ParseUUID(r, "id")
//	if err != nil {
//	    httpx.Error(w, httpx.StatusFor(err), err.Error())
//	    return
//	}
//
// Relaying err.Error() is safe here because the error is InvalidUUIDError,
// written by this package. Do not copy the shape to a store error: those can
// carry driver text naming constraints, tables, and SQLSTATE.
func ParseUUID(r *http.Request, paramName string) (uuid.UUID, error) {
	raw := chi.URLParam(r, paramName)
	if raw == "" {
		raw = r.PathValue(paramName)
	}
	if raw == "" {
		return uuid.Nil, &InvalidUUIDError{Param: paramName}
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, &InvalidUUIDError{Param: paramName, Raw: raw, Err: err}
	}
	return id, nil
}

// QueryInt reads a query parameter as an integer.
// Returns the default value if the parameter is missing.
// Returns InvalidQueryIntError if the parameter is present but not a valid integer.
//
// Usage:
//
//	limit, err := reqparse.QueryInt(r, "limit", 20)
//	if err != nil {
//	    httpx.Error(w, httpx.StatusFor(err), err.Error())
//	    return
//	}
//
// Relaying err.Error() is safe here because the error is InvalidQueryIntError,
// written by this package. Do not copy the shape to a store error.
func QueryInt(r *http.Request, key string, def int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, &InvalidQueryIntError{Key: key, Raw: v, Err: err}
	}
	return n, nil
}

// ClientIP extracts the client IP from a request. It checks X-Forwarded-For
// first (taking the rightmost value, which the most-trusted proxy appends),
// then falls back to r.RemoteAddr.
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := splitXFF(fwd)
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if ip != "" {
				return ip
			}
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func splitXFF(fwd string) []string {
	return strings.Split(fwd, ",")
}

// ClientIPTrusted extracts the client IP from a request with trusted-proxy
// awareness. It prevents X-Forwarded-For spoofing by only honoring XFF when
// the immediate peer (rightmost XFF IP) is in the trustedCIDRs set.
//
// Algorithm:
//   - When trustedCIDRs is nil/empty -> XFF is never trusted. Return RemoteAddr
//     directly (spoof-proof, proxy-blind).
//   - When trustedCIDRs is non-empty -> walk XFF right-to-left:
//   - Rightmost IP must be in trustedCIDRs (the immediate proxy). If not,
//     XFF is spoofed -> return RemoteAddr.
//   - Continue leftward: each IP in trustedCIDRs is an intermediary proxy.
//   - The first IP NOT in trustedCIDRs is the real client -> return it.
//   - If ALL IPs are in trustedCIDRs (no real client found) -> return RemoteAddr.
//   - When XFF header is absent -> return RemoteAddr always.
func ClientIPTrusted(r *http.Request, trustedCIDRs []*net.IPNet) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return remoteIP
	}

	// No trusted proxies configured -> XFF is never trusted.
	if len(trustedCIDRs) == 0 {
		return remoteIP
	}

	// Nor is it trusted from a peer that is not one of them. XFF is honored
	// only when the immediate peer is a trusted proxy, or a client connecting
	// straight to the engine could name any address it liked as soon as a
	// trusted proxy was configured anywhere.
	if !ipInAny(remoteIP, trustedCIDRs) {
		return remoteIP
	}

	parts := splitXFF(fwd)

	// Walk right-to-left (rightmost = immediate peer).
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		parsed := net.ParseIP(ip)
		if parsed == nil {
			// Malformed IP in XFF -> don't trust the chain.
			return remoteIP
		}
		if !ipInAny(ip, trustedCIDRs) {
			// First untrusted IP = real client.
			return ip
		}
		// Trusted proxy IP: continue leftward.
	}

	// All IPs in XFF are trusted proxies -> fallback to RemoteAddr.
	return remoteIP
}

// Pagination

// DefaultPageLimit is the default page size when no limit query parameter is
// specified.
const DefaultPageLimit = 50

// MaxPageLimit is the maximum allowed page size to prevent abuse.
const MaxPageLimit = 500

// QueryOffset reads the `offset` query parameter, defaulting to 0, and rejects
// a negative one with InvalidPaginationError, which maps to 400.
//
// QueryInt alone parses "-1" happily, because it is a valid integer. Passed
// straight into a query, OFFSET -1 is refused by the database, and the refusal
// would reach the caller as a 503 or a 422 for a number the caller typed. It
// is a caller mistake, so it is a 400 like every other bad page parameter.
func QueryOffset(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, &InvalidPaginationError{Field: "offset", Raw: raw}
	}
	return n, nil
}

// ParsePagination extracts offset and limit query parameters with sensible
// defaults (offset=0, limit=DefaultPageLimit). Returns InvalidPaginationError
// for negative, non-integer, or out-of-range values.
//
// Usage:
//
//	offset, limit, err := reqparse.ParsePagination(r)
//	if err != nil {
//	    httpx.Error(w, httpx.StatusFor(err), err.Error())
//	    return
//	}
//
// Relaying err.Error() is safe here because the error is InvalidPaginationError,
// written by this package. Do not copy the shape to a store error.
func ParsePagination(r *http.Request) (offset, limit int, err error) {
	offsetRaw := r.URL.Query().Get("offset")
	limitRaw := r.URL.Query().Get("limit")

	offset = 0
	if offsetRaw != "" {
		n, err := strconv.Atoi(offsetRaw)
		if err != nil {
			return 0, 0, &InvalidPaginationError{Field: "offset", Raw: offsetRaw}
		}
		if n < 0 {
			return 0, 0, &InvalidPaginationError{Field: "offset", Raw: offsetRaw}
		}
		offset = n
	}

	limit = DefaultPageLimit
	if limitRaw != "" {
		n, err := strconv.Atoi(limitRaw)
		if err != nil {
			return 0, 0, &InvalidPaginationError{Field: "limit", Raw: limitRaw}
		}
		if n < 0 {
			return 0, 0, &InvalidPaginationError{Field: "limit", Raw: limitRaw}
		}
		if n > MaxPageLimit {
			n = MaxPageLimit
		}
		limit = n
	}

	return offset, limit, nil
}

// ParseCIDRs parses a list of CIDR strings (e.g. "10.0.0.0/8", "192.168.1.1/32")
// into *net.IPNet pointers suitable for ClientIPTrusted. Returns an error
// when any entry in the list is not a valid CIDR notation.
func ParseCIDRs(raw []string) ([]*net.IPNet, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	cidrs := make([]*net.IPNet, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(r)
		if err != nil {
			return nil, fmt.Errorf("reqparse: invalid CIDR %q: %w", r, err)
		}
		cidrs = append(cidrs, cidr)
	}
	return cidrs, nil
}

// ipInAny reports whether ip parses and falls inside one of the given ranges.
func ipInAny(ip string, cidrs []*net.IPNet) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, cidr := range cidrs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}
