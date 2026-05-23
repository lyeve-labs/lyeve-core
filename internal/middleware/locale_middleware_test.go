package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
)

func TestLocale_ResolvesAcceptLanguage(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   i18n.Locale
	}{
		{"french", "fr", i18n.LocaleFR},
		{"french with quality", "fr;q=0.9, en;q=0.8", i18n.LocaleFR},
		{"english explicit", "en-US, en;q=0.9", i18n.Locale("en-US")},
		{"unsupported falls back to en", "de-DE", i18n.LocaleEN},
		{"empty defaults to en", "", i18n.LocaleEN},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := Locale(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				loc := i18n.LocaleFromCtx(r.Context())
				assert.Equal(t, tt.want, loc)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Language", tt.header)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)
		})
	}
}

func TestLocale_Passthrough(t *testing.T) {
	called := false
	handler := Locale(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "fr")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// Beside the message locale the middleware stashes the content locale: the
// preferred tag as sent, in any language, and nothing when the header is
// absent, so a content read without Accept-Language stays unlocalized
// rather than defaulting to English.
func TestLocale_StashesContentLocale(t *testing.T) {
	for _, tt := range []struct {
		name   string
		header string
		want   string
	}{
		{"unsupported by the catalog still carries", "de-DE", "de-DE"},
		{"quality picks", "en;q=0.5, ja;q=0.9", "ja"},
		{"absent is none", "", ""},
		{"wildcard is none", "*", ""},
		{"not a tag is none", "<script>", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			handler := Locale(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = core.ContentLocaleFromCtx(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("Accept-Language", tt.header)
			}
			handler.ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tt.want, got)
		})
	}
}
