package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLocalizer answers a fixed translation and records what it was asked.
type fakeLocalizer struct {
	schemas []string
	ids     []string
	locales []string
	fail    error
}

func (f *fakeLocalizer) Localize(ctx context.Context, schema, entryID, locale string, data map[string]any) (map[string]any, string, error) {
	if TenantIDFromCtx(ctx) == "" {
		return nil, "", ErrTenantRequired
	}
	f.schemas = append(f.schemas, schema)
	f.ids = append(f.ids, entryID)
	f.locales = append(f.locales, locale)
	if f.fail != nil {
		return nil, "", f.fail
	}
	out := make(map[string]any, len(data)+1)
	for k, v := range data {
		out[k] = v
	}
	out["title"] = "Bonjour"
	return out, "fr", nil
}

var _ ContentLocalizer = (*fakeLocalizer)(nil)

// localizerHost is a stubHost that can hold a localizer, the way the engine
// host does once the owning plugin has registered one.
type localizerHost struct {
	*stubHost
	localizer ContentLocalizer
}

func (h *localizerHost) RegisterContentLocalizer(l ContentLocalizer) { h.localizer = l }
func (h *localizerHost) ContentLocalizer() ContentLocalizer          { return h.localizer }

// The owning plugin registers through its scoped host and a transport
// plugin reads the same localizer through its own. The arguments and the
// result pass through untouched, and clearing the registration is what the
// transport sees as nil.
func TestScopedHost_ContentLocalizer_RegisteredByPluginReachesTransport(t *testing.T) {
	inner := &localizerHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "localization", CapAll)
	graphql := NewScopedHost(inner, "graphql", CapDBWrite|CapRoutes)

	fake := &fakeLocalizer{}
	var reg Host = owner
	r, ok := reg.(ContentLocalizerRegistrar)
	require.True(t, ok, "ScopedHost must satisfy ContentLocalizerRegistrar through the Host interface")
	r.RegisterContentLocalizer(fake)

	var host Host = graphql
	p, ok := host.(ContentLocalizerProvider)
	require.True(t, ok, "ScopedHost must satisfy ContentLocalizerProvider through the Host interface")
	got := p.ContentLocalizer()
	require.Equal(t, ContentLocalizer(fake), got)

	ctx := WithTenantID(context.Background(), "acme")
	out, resolved, err := got.Localize(ctx, "posts", "e1", "fr-CA", map[string]any{"title": "Hello", "body": "x"})
	require.NoError(t, err)
	assert.Equal(t, "fr", resolved)
	assert.Equal(t, "Bonjour", out["title"])
	assert.Equal(t, "x", out["body"])
	assert.Equal(t, []string{"posts"}, fake.schemas)
	assert.Equal(t, []string{"e1"}, fake.ids)
	assert.Equal(t, []string{"fr-CA"}, fake.locales)

	owner.RegisterContentLocalizer(nil)
	assert.Nil(t, graphql.ContentLocalizer(), "after RegisterContentLocalizer(nil) the localizer must be nil")
}

// A bare host answers nil and logs once, with the op named, so a content
// read on an install with no localizer registered serves the source fields
// and says why once.
func TestScopedHost_ContentLocalizer_NilWhenInnerCannotProvide(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "graphql", CapAll)
	for range 3 {
		require.Nil(t, sh.ContentLocalizer())
	}
	recs := unavailableRecords(t, h)
	require.Len(t, recs, 1, h.buf.String())
	assertField(t, recs[0], "op", "ContentLocalizer")
}

func TestValidLocaleTag(t *testing.T) {
	for _, ok := range []string{"en", "fr-CA", "pt_BR", "zh-Hant-TW", "sr-Latn", "de-DE-1996", "es-419"} {
		assert.True(t, ValidLocaleTag(ok), ok)
	}
	for _, bad := range []string{"", "f", "fr-", "-fr", "fr CA", "fr;q=0.9", "français", "en-US-" + string(make([]byte, 9)), "abcdefghijklmnopqrstuvwxyzabcdefghij"} {
		assert.False(t, ValidLocaleTag(bad), "%q", bad)
	}
}

// The explicit locale wins, the header's preference is next, and nothing
// is last. Only the explicit value is shape-checked, since the header's
// was shaped by the middleware.
func TestResolveLocale_Order(t *testing.T) {
	bare := context.Background()
	viaHeader := WithContentLocale(bare, "de")

	got, err := ResolveLocale(viaHeader, "fr")
	require.NoError(t, err)
	assert.Equal(t, "fr", got)

	got, err = ResolveLocale(viaHeader, " fr-CA ")
	require.NoError(t, err)
	assert.Equal(t, "fr-CA", got)

	got, err = ResolveLocale(viaHeader, "")
	require.NoError(t, err)
	assert.Equal(t, "de", got)

	got, err = ResolveLocale(bare, "")
	require.NoError(t, err)
	assert.Equal(t, "", got)

	_, err = ResolveLocale(viaHeader, "not a tag")
	assert.True(t, errors.Is(err, ErrInvalidLocale))
}

func TestRequestedLocale_QueryThenContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/posts?locale=it", nil)
	r = r.WithContext(WithContentLocale(r.Context(), "de"))
	got, err := RequestedLocale(r)
	require.NoError(t, err)
	assert.Equal(t, "it", got)

	r = httptest.NewRequest(http.MethodGet, "/api/v1/content/posts", nil)
	r = r.WithContext(WithContentLocale(r.Context(), "de"))
	got, err = RequestedLocale(r)
	require.NoError(t, err)
	assert.Equal(t, "de", got)

	r = httptest.NewRequest(http.MethodGet, "/api/v1/content/posts?locale=%3Cb%3E", nil)
	_, err = RequestedLocale(r)
	assert.True(t, errors.Is(err, ErrInvalidLocale))
}
