package middleware

import (
	"net/http"
	"strings"
)

// RequireJSONContentType returns middleware that rejects mutation requests
// (POST, PUT, PATCH) whose Content-Type is not application/json or
// multipart/form-data.
//
// GET, DELETE, HEAD, OPTIONS, and TRACE pass through untouched: they have
// no body (or the body is ignored).
//
// Multipart/form-data is allowed so file-upload endpoints (media, avatars,
// etc.) can accept multipart payloads without special-casing.
//
// Requests with no Content-Type header on mutation verbs receive 415
// Unsupported Media Type: attackers sometimes omit it to bypass
// content-sniffing guards.
//
// AllowMediaTypes and AllowMediaTypesWhen admit further media types on one
// path or one route, for a route whose body is legitimately not JSON. Every
// other path keeps the rule.
func RequireJSONContentType(opts ...ContentTypeOption) func(http.Handler) http.Handler {
	rules := &contentTypeRules{exact: map[string]map[string]bool{}}
	for _, o := range opts {
		o(rules)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch:
				ct := r.Header.Get("Content-Type")
				// A request with no entity has no media type to declare, and
				// plenty of admin actions are a bare POST: rollback, reset,
				// archive, restore. Refusing those with 415 refuses a request
				// that was never malformed. Handlers that do need a body still
				// fail their own decode.
				if ct == "" && r.ContentLength == 0 {
					next.ServeHTTP(w, r)
					return
				}
				// media-type is before the semicolon (charset, boundary, etc.)
				mediaType, _, _ := strings.Cut(ct, ";")
				mediaType = strings.TrimSpace(strings.ToLower(mediaType))

				if mediaType == "application/json" || mediaType == "multipart/form-data" || rules.allows(r, mediaType) {
					next.ServeHTTP(w, r)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnsupportedMediaType)
				w.Write([]byte(`{"error":"unsupported media type, expected application/json or multipart/form-data"}`)) //nolint:errcheck // best-effort error body. Client may be disconnected
				return
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// ContentTypeOption widens RequireJSONContentType for specific paths.
type ContentTypeOption func(*contentTypeRules)

// contentTypeRules holds the media types admitted beyond JSON and multipart.
// exact is keyed by path for any method. admit is asked for everything else,
// by a caller that can resolve which route the request is addressed to.
type contentTypeRules struct {
	exact map[string]map[string]bool
	admit []func(r *http.Request, mediaType string) bool
}

func (c *contentTypeRules) allows(r *http.Request, mediaType string) bool {
	if c.exact[r.URL.Path][mediaType] {
		return true
	}
	for _, fn := range c.admit {
		if fn(r, mediaType) {
			return true
		}
	}
	return false
}

// AllowMediaTypes admits the given media types, compared case-insensitively
// and without parameters, on requests whose path is exactly path.
func AllowMediaTypes(path string, mediaTypes ...string) ContentTypeOption {
	return func(c *contentTypeRules) {
		if c.exact[path] == nil {
			c.exact[path] = map[string]bool{}
		}
		for _, t := range mediaTypes {
			c.exact[path][strings.ToLower(strings.TrimSpace(t))] = true
		}
	}
}

// AllowMediaTypesWhen admits a media type, lower-cased and without
// parameters, on the requests admit accepts. The guard runs ahead of
// routing, so it cannot tell which route a path reaches. A router that can
// answer that question for itself decides here, and nil admits nothing.
func AllowMediaTypesWhen(admit func(r *http.Request, mediaType string) bool) ContentTypeOption {
	return func(c *contentTypeRules) {
		if admit != nil {
			c.admit = append(c.admit, admit)
		}
	}
}
