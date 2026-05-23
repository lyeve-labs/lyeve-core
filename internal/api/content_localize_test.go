package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// recordingLocalizer stands in for a registered localizer: it translates
// body into French for any fr locale, falls back to the source fields in
// "en" for every other locale, and fails on demand.
type recordingLocalizer struct {
	schemas []string
	ids     []string
	locales []string
	fail    error
}

func (f *recordingLocalizer) Localize(_ context.Context, schema, entryID, locale string, data map[string]any) (map[string]any, string, error) {
	f.schemas = append(f.schemas, schema)
	f.ids = append(f.ids, entryID)
	f.locales = append(f.locales, locale)
	if f.fail != nil {
		return nil, "", f.fail
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	if strings.HasPrefix(locale, "fr") {
		out["body"] = "premier"
		return out, "fr", nil
	}
	return out, "en", nil
}

// localizerProvider is the host as the content handler sees it: whatever
// the plugin registered last, nil included.
type localizerProvider struct{ l core.ContentLocalizer }

func (p *localizerProvider) ContentLocalizer() core.ContentLocalizer { return p.l }

// localizedFixture is the schema fixture with one note posted and a
// second handler over the same store that can reach a localizer.
type localizedFixture struct {
	schemaContentFixture
	id        string
	localizer *recordingLocalizer
	provider  *localizerProvider
	localized *ContentHandler
}

func newLocalizedFixture(t *testing.T, fx schemaContentFixture) localizedFixture {
	t.Helper()
	id := createdID(t, fx.create(t, "notes", map[string]any{
		"title": "First", "body": "first",
	}))
	loc := &recordingLocalizer{}
	prov := &localizerProvider{l: loc}
	h := *fx.content
	h.localizer = prov
	return localizedFixture{schemaContentFixture: fx, id: id, localizer: loc, provider: prov, localized: &h}
}

// read serves one GET through h with the given query and headers. The
// request runs through the Locale middleware, as it does in the router, so
// Accept-Language reaches the handler the way it does in production.
func (fx localizedFixture) read(t *testing.T, path, query string, headers map[string]string, params map[string]string, fn http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path+query, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = chiCtx(r.WithContext(fx.ctx), params)
	rr := httptest.NewRecorder()
	apimw.Locale(fn).ServeHTTP(rr, r)
	return rr
}

func (fx localizedFixture) get(t *testing.T, h *ContentHandler, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.read(t, "/api/v1/content/notes/"+fx.id, query, headers, map[string]string{"schema": "notes", "id": fx.id}, h.Get)
}

func (fx localizedFixture) list(t *testing.T, h *ContentHandler, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.read(t, "/api/v1/content/notes", query, headers, map[string]string{"schema": "notes"}, h.List)
}

func (fx localizedFixture) cursor(t *testing.T, h *ContentHandler, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.read(t, "/api/v1/content/notes/cursor", query, headers, map[string]string{"schema": "notes"}, h.ListCursor)
}

type localizedRow struct {
	ID             string         `json:"id"`
	Data           map[string]any `json:"data"`
	ResolvedLocale *string        `json:"resolved_locale"`
}

func decodeRow(t *testing.T, rr *httptest.ResponseRecorder) localizedRow {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var row localizedRow
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &row), rr.Body.String())
	return row
}

func decodeRows(t *testing.T, rr *httptest.ResponseRecorder) []localizedRow {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var rows []localizedRow
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &rows), rr.Body.String())
	return rows
}

// A content read with a locale hands each row to the registered localizer
// and answers what it returned, with the locale it resolved to beside the
// data. The query parameter wins over Accept-Language, the header serves
// when the parameter is absent, and a locale the localizer cannot serve
// still answers, in the locale it fell back to.
func TestContentHandler_Read_LocalizesThroughTheRegisteredLocalizer(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, base schemaContentFixture) {
		fx := newLocalizedFixture(t, base)

		t.Run("get with ?locale merges the translation", func(t *testing.T) {
			rr := fx.get(t, fx.localized, "?locale=fr", nil)
			row := decodeRow(t, rr)
			assert.Equal(t, "premier", row.Data["body"])
			assert.Equal(t, "First", row.Data["title"], "untranslated fields stay")
			require.NotNil(t, row.ResolvedLocale)
			assert.Equal(t, "fr", *row.ResolvedLocale)
			assert.Equal(t, "Accept-Language", rr.Header().Get("Vary"))
			n := len(fx.localizer.locales)
			require.Greater(t, n, 0)
			assert.Equal(t, "notes", fx.localizer.schemas[n-1])
			assert.Equal(t, fx.id, fx.localizer.ids[n-1])
			assert.Equal(t, "fr", fx.localizer.locales[n-1])
		})

		t.Run("list localizes every row", func(t *testing.T) {
			rows := decodeRows(t, fx.list(t, fx.localized, "?locale=fr-CA", nil))
			require.Len(t, rows, 1)
			assert.Equal(t, "premier", rows[0].Data["body"])
			require.NotNil(t, rows[0].ResolvedLocale)
			assert.Equal(t, "fr", *rows[0].ResolvedLocale)
			assert.Equal(t, "fr-CA", fx.localizer.locales[len(fx.localizer.locales)-1])
		})

		t.Run("cursor list localizes every row", func(t *testing.T) {
			rr := fx.cursor(t, fx.localized, "?locale=fr", nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var page struct {
				Data []localizedRow `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
			require.Len(t, page.Data, 1)
			assert.Equal(t, "premier", page.Data[0].Data["body"])
			require.NotNil(t, page.Data[0].ResolvedLocale)
			assert.Equal(t, "fr", *page.Data[0].ResolvedLocale)
		})

		t.Run("Accept-Language serves when the parameter is absent", func(t *testing.T) {
			row := decodeRow(t, fx.get(t, fx.localized, "", map[string]string{"Accept-Language": "fr-CH, en;q=0.5"}))
			assert.Equal(t, "premier", row.Data["body"])
			assert.Equal(t, "fr-CH", fx.localizer.locales[len(fx.localizer.locales)-1])
		})

		t.Run("the parameter wins over the header", func(t *testing.T) {
			row := decodeRow(t, fx.get(t, fx.localized, "?locale=de", map[string]string{"Accept-Language": "fr"}))
			assert.Equal(t, "first", row.Data["body"])
			require.NotNil(t, row.ResolvedLocale)
			assert.Equal(t, "en", *row.ResolvedLocale, "the fallback the localizer resolved to")
			assert.Equal(t, "de", fx.localizer.locales[len(fx.localizer.locales)-1])
		})

		t.Run("a locale that is not a tag is refused", func(t *testing.T) {
			rr := fx.get(t, fx.localized, "?locale=%3Cb%3E", nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			rr = fx.list(t, fx.localized, "?locale=fr%20CA", nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
		})

		t.Run("a localizer failure is a 503 with a static message", func(t *testing.T) {
			fx.localizer.fail = errors.New("dial tcp: connection refused")
			defer func() { fx.localizer.fail = nil }()
			for _, rr := range []*httptest.ResponseRecorder{
				fx.get(t, fx.localized, "?locale=fr", nil),
				fx.list(t, fx.localized, "?locale=fr", nil),
				fx.cursor(t, fx.localized, "?locale=fr", nil),
			} {
				assert.Equal(t, http.StatusServiceUnavailable, rr.Code, rr.Body.String())
				assert.Contains(t, rr.Body.String(), "localization unavailable")
				assert.NotContains(t, rr.Body.String(), "connection refused")
			}
		})
	})
}

// With no localizer registered, or with one registered and no locale in the
// request, a read answers as an unlocalized read does: no resolved_locale,
// no translation, and the locale parameter and header
// ignored rather than refused. Bodies are compared as JSON because the
// encoder writes a data map in iteration order.
func TestContentHandler_Read_UnchangedWithoutLocalizerOrLocale(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, base schemaContentFixture) {
		fx := newLocalizedFixture(t, base)
		plain := fx.content
		unregistered := *fx.content
		unregistered.localizer = &localizerProvider{}

		baselineGet := fx.get(t, plain, "", nil)
		baselineList := fx.list(t, plain, "", nil)
		require.Equal(t, http.StatusOK, baselineGet.Code, baselineGet.Body.String())
		require.Equal(t, http.StatusOK, baselineList.Code, baselineList.Body.String())
		assert.NotContains(t, baselineGet.Body.String(), "resolved_locale")
		assert.NotContains(t, baselineList.Body.String(), "resolved_locale")
		assert.Empty(t, baselineGet.Header().Get("Vary"))

		for name, h := range map[string]*ContentHandler{
			"no provider":                   plain,
			"provider with no registration": &unregistered,
		} {
			t.Run(name, func(t *testing.T) {
				for _, q := range []string{"", "?locale=fr", "?locale=%3Cb%3E"} {
					for _, hdr := range []map[string]string{nil, {"Accept-Language": "fr"}} {
						rr := fx.get(t, h, q, hdr)
						assert.JSONEq(t, baselineGet.Body.String(), rr.Body.String(), "get %q %v", q, hdr)
						assert.Empty(t, rr.Header().Get("Vary"), "get %q %v", q, hdr)
						rr = fx.list(t, h, q, hdr)
						assert.JSONEq(t, baselineList.Body.String(), rr.Body.String(), "list %q %v", q, hdr)
					}
				}
			})
		}
		assert.Empty(t, fx.localizer.locales, "no read reached the localizer")

		t.Run("registered but no locale in the request", func(t *testing.T) {
			rr := fx.get(t, fx.localized, "", nil)
			assert.JSONEq(t, baselineGet.Body.String(), rr.Body.String())
			assert.Equal(t, "Accept-Language", rr.Header().Get("Vary"), "the response now depends on the header")
			rr = fx.list(t, fx.localized, "", nil)
			assert.JSONEq(t, baselineList.Body.String(), rr.Body.String())
			assert.Empty(t, fx.localizer.locales, "no locale, no call")
		})
	})
}
