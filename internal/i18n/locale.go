package i18n

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// Locale is a BCP 47 language tag (e.g. "en", "fr", "en-US", "fr-CA").
type Locale string

// Primary returns the primary language subtag ("en" from "en-US").
func (l Locale) Primary() string {
	s := string(l)
	if i := strings.IndexByte(s, '-'); i != -1 {
		return s[:i]
	}
	return s
}

// String returns the full locale tag.
func (l Locale) String() string { return string(l) }

// Supported locales. The zero-value (empty string) is treated as English.
const (
	LocaleEN Locale = "en"
	LocaleFR Locale = "fr"
)

// Fallback chain per primary language. Unlisted languages fall back to "en".
var fallbackChain = map[string][]string{
	"fr": {"fr", "en"},
}

// Default is returned when the request carries no Accept-Language header.
const Default = "en"

// Accept-Language parsing

// AcceptLanguage is a single entry in the Accept-Language header.
type AcceptLanguage struct {
	Locale Locale
	Q      float64 // quality value, 0.0-1.0
}

// ParseAcceptLanguage parses an RFC 7231 §5.3.5 Accept-Language header value
// and returns the locales sorted by quality (descending), tie-broken by
// position in the header. When the header is empty, returns [{LocaleEN, 1.0}].
//
// Examples:
//
//	"fr-CH, fr;q=0.9, en;q=0.8" -> [{fr-CH 1.0}, {fr 0.9}, {en 0.8}]
//	"*"                          -> [{en 0.5}]
//	""                           -> [{en 1.0}]
func ParseAcceptLanguage(header string) []AcceptLanguage {
	header = strings.TrimSpace(header)
	if header == "" {
		return []AcceptLanguage{{Locale: LocaleEN, Q: 1.0}}
	}

	var al []AcceptLanguage
	for i, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		entry := AcceptLanguage{Q: 1.0}

		if idx := strings.IndexByte(part, ';'); idx != -1 {
			entry.Locale = Locale(strings.TrimSpace(part[:idx]))
			qPart := strings.TrimSpace(part[idx+1:])
			if strings.HasPrefix(qPart, "q=") || strings.HasPrefix(qPart, "Q=") {
				if q, err := strconv.ParseFloat(qPart[2:], 64); err == nil {
					entry.Q = q
				}
			}
		} else {
			entry.Locale = Locale(part)
		}

		// RFC 7231 §5.3.1: q=0 means "not acceptable". Skip entirely.
		if entry.Q <= 0 {
			continue
		}

		// Wildcard "*": treat as low-priority English per RFC 7231.
		if string(entry.Locale) == "*" {
			if entry.Q == 1.0 {
				entry.Q = 0.5
			}
			entry.Locale = LocaleEN
		}

		entry.Q -= float64(i) * 0.0001 // tie-break by position

		al = append(al, entry)
	}

	sort.Slice(al, func(i, j int) bool {
		return al[i].Q > al[j].Q
	})

	if len(al) == 0 {
		return []AcceptLanguage{{Locale: LocaleEN, Q: 1.0}}
	}
	return al
}

// Resolve picks the best matching locale from a quality-sorted Accept-Language
// list. Returns the first locale whose primary subtag is in supported.
// Falls back to LocaleEN when no match is found.
func Resolve(acceptLang []AcceptLanguage, supported map[string]bool) Locale {
	for _, ae := range acceptLang {
		primary := ae.Locale.Primary()
		if _, ok := supported[primary]; ok {
			return ae.Locale
		}
	}
	return LocaleEN
}

// Request context

type localeCtxKey struct{}

// WithLocale returns a new context carrying the resolved locale.
func WithLocale(ctx context.Context, loc Locale) context.Context {
	return context.WithValue(ctx, localeCtxKey{}, loc)
}

// LocaleFromCtx returns the resolved locale from context, or LocaleEN when unset.
func LocaleFromCtx(ctx context.Context) Locale {
	if v, ok := ctx.Value(localeCtxKey{}).(Locale); ok && v != "" {
		return v
	}
	return LocaleEN
}

// PreferredTag returns the language tag the header prefers most, or "" when
// the header is absent, names only the wildcard, or accepts nothing. Unlike
// Resolve it does not collapse the tag onto the languages the engine's own
// messages are translated into: a content read in "de" is the registered
// content localizer's to answer, whatever the message catalog speaks.
func PreferredTag(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	best, bestQ := "", -1.0
	for i, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, q := part, 1.0
		if idx := strings.IndexByte(part, ';'); idx != -1 {
			tag = strings.TrimSpace(part[:idx])
			qPart := strings.TrimSpace(part[idx+1:])
			if strings.HasPrefix(qPart, "q=") || strings.HasPrefix(qPart, "Q=") {
				if v, err := strconv.ParseFloat(qPart[2:], 64); err == nil {
					q = v
				}
			}
		}
		if tag == "*" || q <= 0 {
			continue
		}
		q -= float64(i) * 0.0001 // tie-break by position
		if q > bestQ {
			best, bestQ = tag, q
		}
	}
	return best
}
