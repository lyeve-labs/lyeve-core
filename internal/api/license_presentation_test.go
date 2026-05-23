package api

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
)

// A licensing implementation mounts its presentation with its other routes.
// Any signed-in role reads it, and the document describes it in the words
// pkg/licensing gives the route, and only while it is served.
func TestLicensePresentation_ServedToEveryRoleAndDocumentedWhileServed(t *testing.T) {
	route := licensing.PresentationRoute(func(context.Context) licensing.Presentation {
		return licensing.Presentation{Renew: true, Links: []licensing.Link{
			{Rel: licensing.RelSupport, Label: "Support", URL: "https://help.example.com/"},
		}}
	})
	newAdmin := func(opts ...RouterOption) http.Handler {
		h, err := NewAdminRouter(&fakeDB{engine: "postgres", queryRowFactory: newTokenVersionRow}, testConfig(),
			append([]RouterOption{WithLifetime(testLifetime(t))}, opts...)...)
		require.NoError(t, err)
		return h
	}
	get := func(h http.Handler, path string, roles ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if roles != nil {
			req.Header.Set("Authorization", "Bearer "+bearerWithRoles(t, roles...))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	documented := func(h http.Handler) map[string]any {
		rec := get(h, "/api/admin/openapi.json", "super_admin")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var doc struct {
			Paths map[string]map[string]map[string]any `json:"paths"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
		return doc.Paths[licensing.PresentationPath]["get"]
	}

	served := newAdmin(WithLicenseRoutes("license", []core.RouteDecl{route}))
	for _, role := range []string{"editor", "admin", "super_admin"} {
		rec := get(served, licensing.PresentationPath, role)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", role, rec.Body.String())
		assert.JSONEq(t, `{"links":[{"rel":"support","label":"Support","url":"https://help.example.com/"}],"renew":true}`, rec.Body.String(), role)
	}
	assert.Equal(t, http.StatusUnauthorized, get(served, licensing.PresentationPath).Code)

	op := documented(served)
	require.NotNil(t, op, "a served presentation is documented")
	assert.Equal(t, []any{"Admin / License"}, op["tags"])
	assert.Equal(t, route.Doc.Summary, op["summary"])
	assert.NotContains(t, op, xRoles, "any signed-in role reads it")
	assert.NotContains(t, op, xFeature, "the licensing implementation is no license feature")

	assert.Nil(t, documented(newAdmin()), "a build that serves no presentation documents none")
}
