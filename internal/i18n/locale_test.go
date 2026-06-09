package i18n

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAcceptLanguage(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected []AcceptLanguage
	}{
		{
			name:   "empty",
			header: "",
			expected: []AcceptLanguage{
				{Locale: LocaleEN, Q: 1.0},
			},
		},
		{
			name:   "single locale",
			header: "fr",
			expected: []AcceptLanguage{
				{Locale: "fr", Q: 0.9999},
			},
		},
		{
			name:   "locale with region",
			header: "fr-CA",
			expected: []AcceptLanguage{
				{Locale: "fr-CA", Q: 0.9999},
			},
		},
		{
			name:   "multiple with quality",
			header: "fr-CH, fr;q=0.9, en;q=0.8",
			expected: []AcceptLanguage{
				{Locale: "fr-CH", Q: 0.9999},
				{Locale: "fr", Q: 0.8998999999999999},
				{Locale: "en", Q: 0.7998},
			},
		},
		{
			name:   "wildcard",
			header: "*",
			expected: []AcceptLanguage{
				{Locale: LocaleEN, Q: 0.4999},
			},
		},
		{
			name:   "wildcard with quality tiebreak",
			header: "de;q=0.9, *;q=0.5, en;q=0.8",
			expected: []AcceptLanguage{
				{Locale: "de", Q: 0.8998999999999999},
				{Locale: LocaleEN, Q: 0.7998},
				{Locale: LocaleEN, Q: 0.4998},
			},
		},
		{
			name:   "q=0 filtered out per RFC 7231",
			header: "fr;q=1.0, de;q=0, en;q=0.8",
			expected: []AcceptLanguage{
				{Locale: "fr", Q: 0.9999},
				{Locale: "en", Q: 0.7998},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseAcceptLanguage(tt.header)
			require.Equal(t, len(tt.expected), len(got), "expected %d entries, got %d", len(tt.expected), len(got))
			for i := range tt.expected {
				assert.Equal(t, tt.expected[i].Locale, got[i].Locale, "locale mismatch at position %d", i)
				assert.InDelta(t, tt.expected[i].Q, got[i].Q, 0.001, "quality mismatch at position %d", i)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	supported := map[string]bool{"en": true, "fr": true}

	tests := []struct {
		name   string
		header string
		want   Locale
	}{
		{"fr primary", "fr", LocaleFR},
		{"fr-CH falls back to fr", "fr-CH, fr;q=0.9, en;q=0.8", Locale("fr-CH")},
		{"en explicit", "en-US", Locale("en-US")},
		{"de unsupported falls back to en", "de-DE, en;q=0.8", LocaleEN},
		{"empty defaults to en", "", LocaleEN},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			al := ParseAcceptLanguage(tt.header)
			got := Resolve(al, supported)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLocaleFromCtx(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	loc := LocaleFromCtx(req.Context())
	assert.Equal(t, LocaleEN, loc)

	ctx := WithLocale(req.Context(), LocaleFR)
	loc = LocaleFromCtx(ctx)
	assert.Equal(t, LocaleFR, loc)
}

func TestLocPrimary(t *testing.T) {
	assert.Equal(t, "fr", Locale("fr-CA").Primary())
	assert.Equal(t, "en", Locale("en").Primary())
	assert.Equal(t, "zh", Locale("zh-Hans-CN").Primary())
}

func TestLocaleString(t *testing.T) {
	assert.Equal(t, "fr-CA", Locale("fr-CA").String())
}

// The content locale is the tag the caller preferred, in any language, and
// nothing when the header is absent or only the wildcard: the message
// catalog's fallback to English must not become a content read's locale.
func TestPreferredTag(t *testing.T) {
	for _, tt := range []struct {
		header string
		want   string
	}{
		{"", ""},
		{"*", ""},
		{"*;q=0.1", ""},
		{"de", "de"},
		{"de-DE", "de-DE"},
		{"fr-CH, fr;q=0.9, en;q=0.8", "fr-CH"},
		{"en;q=0.8, fr;q=0.9", "fr"},
		{"en;q=0, fr;q=0.5", "fr"},
		{"*, ja;q=0.5", "ja"},
		{"pt-BR;Q=0.7, es", "es"},
		{" , ,de", "de"},
	} {
		assert.Equal(t, tt.want, PreferredTag(tt.header), "%q", tt.header)
	}
}
