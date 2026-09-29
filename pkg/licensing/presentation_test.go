package licensing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// The route carries the policy every route under the license path carries:
// any signed-in role reads it, its bodies stay out of request capture, and no
// admin token reaches it.
func TestPresentationRoute_DeclaresThePolicyOfTheLicensePath(t *testing.T) {
	route := licensing.PresentationRoute(func(context.Context) licensing.Presentation { return licensing.Presentation{} })

	assert.Equal(t, http.MethodGet, route.Method)
	assert.Equal(t, "/api/admin/license", route.Pattern)
	assert.Equal(t, licensing.PresentationPath, route.Pattern)
	assert.Equal(t, core.GroupAuth, route.Group)
	assert.True(t, route.Sensitive)
	assert.True(t, route.SessionOnly)
	assert.Empty(t, route.AdminGrant)
	assert.True(t, core.IsSessionOnlyAdminRoute(route.Method, route.Pattern, route))
	require.NotNil(t, route.Doc)
	assert.Equal(t, "Admin / License", route.Doc.Tag)
	plugintest.AssertRouteContract(t, "license", []core.RouteDecl{route})
}

// serve answers one request to the route fn backs, with ctx as the request's
// context.
func serve(t *testing.T, ctx context.Context, fn func(context.Context) licensing.Presentation) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, licensing.PresentationPath, nil).WithContext(ctx)
	licensing.PresentationRoute(fn).Handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec, body
}

type ctxKey struct{}

func TestPresentationRoute_ServesWhatTheImplementationSays(t *testing.T) {
	var asked context.Context
	ctx := context.WithValue(context.Background(), ctxKey{}, "the request's")
	rec, body := serve(t, ctx, func(c context.Context) licensing.Presentation {
		asked = c
		return licensing.Presentation{Renew: true, Links: []licensing.Link{
			{Rel: licensing.RelUpgrade, Label: "View plans", URL: "/admin/settings/license"},
			{Rel: licensing.RelPurchase, Label: "Buy", URL: "https://shop.example.com/buy"},
			{Rel: licensing.RelSupport, Label: "Support", URL: "HTTPS://help.example.com/"},
		}}
	})

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.NotNil(t, asked)
	assert.Equal(t, "the request's", asked.Value(ctxKey{}), "the implementation is asked with the request's context")
	assert.JSONEq(t, `true`, string(body["renew"]))
	assert.JSONEq(t, `[
		{"rel":"upgrade","label":"View plans","url":"/admin/settings/license"},
		{"rel":"purchase","label":"Buy","url":"https://shop.example.com/buy"},
		{"rel":"support","label":"Support","url":"HTTPS://help.example.com/"}
	]`, string(body["links"]))
}

// A console renders each URL as a link, so only an https URL or a path on the
// console reaches it.
func TestPresentationRoute_LeavesOutALinkThatIsNotHTTPSOrAConsolePath(t *testing.T) {
	for _, bad := range []string{
		"", "javascript:alert(1)", "data:text/html,hi", "http://example.com/", "https://", "https:///path",
		"//example.com/path", `/\example.com`, "/admin/\npath", "ftp://example.com/", "example.com/path", "mailto:help@example.com",
	} {
		t.Run(bad, func(t *testing.T) {
			_, body := serve(t, context.Background(), func(context.Context) licensing.Presentation {
				return licensing.Presentation{Links: []licensing.Link{
					{Rel: licensing.RelDocs, Label: "Refused", URL: bad},
					{Rel: licensing.RelDocs, Label: "Kept", URL: "https://docs.example.com/license/"},
				}}
			})
			assert.JSONEq(t, `[{"rel":"docs","label":"Kept","url":"https://docs.example.com/license/"}]`, string(body["links"]))
		})
	}
}

func TestPresentationRoute_NeverSendsNullLinks(t *testing.T) {
	for name, fn := range map[string]func(context.Context) licensing.Presentation{
		"an empty presentation": func(context.Context) licensing.Presentation { return licensing.Presentation{} },
		"no function":           nil,
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := serve(t, context.Background(), fn)
			assert.JSONEq(t, `{"links":[],"renew":false}`, rec.Body.String())
		})
	}
}
