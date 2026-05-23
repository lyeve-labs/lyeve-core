package core

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// ContentLocalizer answers a content read in a locale. The engine stores one
// set of fields per entry. The translations live with the plugin that owns
// them, so the engine asks that plugin to merge them in at read time rather
// than reading a table it does not own. The content handlers call it for
// every row a locale-aware read returns, and a transport that serves
// content on another wire (GraphQL, gRPC) calls it the same way.
//
// Localize takes the entry's source fields and returns the fields to serve
// and the locale they came from: the requested locale when a translation
// exists, a fallback the tenant configured, or the source locale when
// nothing else does. The engine never inspects resolved beyond writing it
// into the response as resolved_locale. data is the engine's own map and
// may be returned as is or replaced. A localizer never mutates it.
//
// An error is a store failure, nothing else: a missing translation is a
// fallback, not an error. The handler answers a failure as 503, since the
// read cannot be served correctly and serving the source fields under a
// locale the caller asked for would be wrong silently.
type ContentLocalizer interface {
	Localize(ctx context.Context, schema, entryID, locale string, data map[string]any) (map[string]any, string, error)
}

// ContentLocalizerRegistrar is implemented by the engine host. The plugin
// that owns translations calls it when it starts and again with nil when
// it stops, so the registration follows the license and the plugin's
// lifecycle, not the process.
type ContentLocalizerRegistrar interface {
	RegisterContentLocalizer(l ContentLocalizer)
}

// ContentLocalizerProvider is implemented by the engine host and forwarded
// by ScopedHost. The content handlers and any transport plugin read it per
// request, never at Start. Nil means no plugin has registered a localizer:
// a locale in the request is then ignored and the response carries the
// source fields.
type ContentLocalizerProvider interface {
	ContentLocalizer() ContentLocalizer
}

// ErrInvalidLocale is answered by ResolveLocale and RequestedLocale when
// the caller named a locale that is not a language tag. A handler answers
// it as 400.
var ErrInvalidLocale = errors.New("invalid locale")

// localeTagRe is the shape of a locale a caller may ask for: a language
// subtag with optional script, region and variant subtags, joined by a
// hyphen or an underscore. It is a shape check, not a registry lookup: the
// plugin that owns translations decides which locales a tenant has.
var localeTagRe = regexp.MustCompile(`^[A-Za-z]{2,8}([-_][A-Za-z0-9]{1,8})*$`)

// maxLocaleTagLen bounds what a query parameter can push through the
// regexp. The longest tags in use are under half of it.
const maxLocaleTagLen = 35

// ValidLocaleTag reports whether tag has the shape of a locale.
func ValidLocaleTag(tag string) bool {
	return len(tag) <= maxLocaleTagLen && localeTagRe.MatchString(tag)
}

type contentLocaleCtxKey struct{}

// WithContentLocale stashes the locale a request asked for through
// Accept-Language on the context. The engine's middleware calls it with
// the header's most preferred tag, and only when the header names one.
// The engine's own message catalog resolves the header separately and
// falls back to English, which is not what a content read should do.
func WithContentLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, contentLocaleCtxKey{}, locale)
}

// ContentLocaleFromCtx returns the Accept-Language preference the
// middleware stashed, or "" when the request carried no usable header.
func ContentLocaleFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(contentLocaleCtxKey{}).(string)
	return v
}

// ResolveLocale is the one resolution order every transport shares: the
// locale the caller named explicitly (a query parameter, a GraphQL
// argument, a gRPC field) when present, else the Accept-Language
// preference on ctx, else "". An explicit value that is not a language tag
// is ErrInvalidLocale. The header's value was already shaped by the
// middleware and is never refused.
func ResolveLocale(ctx context.Context, explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		if !ValidLocaleTag(explicit) {
			return "", ErrInvalidLocale
		}
		return explicit, nil
	}
	return ContentLocaleFromCtx(ctx), nil
}

// RequestedLocale is ResolveLocale for an HTTP request: the locale query
// parameter, else the Accept-Language preference, else "".
func RequestedLocale(r *http.Request) (string, error) {
	return ResolveLocale(r.Context(), r.URL.Query().Get("locale"))
}
