package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaMask grants every role every action and hides the listed fields of
// each schema it names, so a test can mask the source of a relation and leave
// its target alone.
type schemaMask map[string][]string

func (schemaMask) Allowed(context.Context, []string, string, string) (bool, error) {
	return true, nil
}

func (m schemaMask) FieldMask(_ context.Context, _ []string, schema string) ([]string, error) {
	return m[schema], nil
}

// The relation route asked for read on both schemas and applied the target's
// mask, and never the source's. A caller whose rule hides the field on the
// source, and who may read the target, listed the field's items and so
// learned which records the hidden field links to.
func TestContentHandler_ListRelations_HonorsTheSourceMask(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"articles"}, func(t *testing.T, fx schemaContentFixture) {
		label := createdID(t, fx.create(t, "labels", map[string]any{"name": "linked label"}))
		article := createdID(t, fx.create(t, "articles", map[string]any{"title": "a"}))
		target := "/api/v1/content/articles/" + article + "/relations/tags"
		params := map[string]string{"schema": "articles", "id": article, "field": "tags"}
		rr := fx.send(t, fx.ctx, http.MethodPut, target, params, map[string]any{"ids": []string{label}}, fx.content.SetRelations)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		list := func() *httptest.ResponseRecorder {
			return fx.send(t, asEditor(fx.ctx), http.MethodGet, target, params, nil, fx.content.ListRelations)
		}

		fx.content.perms = fakeProvider{schemaMask{"labels": {"secret"}}}
		rr = list()
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), label, "a mask on the target alone hid the relation")

		fx.content.perms = fakeProvider{schemaMask{"articles": {"tags"}}}
		rr = list()
		assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), label)
	})
}
