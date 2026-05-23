package middleware

import (
	"net/http"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// supportedLocales is the set of languages for which translations exist.
// Must stay in sync with translations.go. When a new language is added,
// add its primary subtag here.
var supportedLocales = map[string]bool{
	"en": true,
	"fr": true,
}

// Locale parses Accept-Language, resolves the best matching locale from
// supportedLocales, and injects it into the request context. Must run after
// ClientAddress. Safe early in the chain.
//
// The header is read twice on purpose. The message locale falls back to
// English and only ever names a language the engine's own messages exist
// in. A content read wants the tag the caller actually asked for, in any
// language, and nothing at all when the header is absent, so the header's
// preferred tag is stashed separately through core.WithContentLocale and
// only when it names one. A shape it cannot carry is dropped there rather
// than refused, since a browser sends the header on every request.
func Locale(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Accept-Language")
		al := i18n.ParseAcceptLanguage(header)
		loc := i18n.Resolve(al, supportedLocales)
		ctx := i18n.WithLocale(r.Context(), loc)
		if tag := i18n.PreferredTag(header); tag != "" && core.ValidLocaleTag(tag) {
			ctx = core.WithContentLocale(ctx, tag)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
