package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
)

// writerOnlyChecker lets every role read and only super_admin write, the
// shape of an install where the public reads a schema its editors maintain.
type writerOnlyChecker struct{}

func (writerOnlyChecker) Allowed(_ context.Context, roles []string, _ string, action string) (bool, error) {
	return action == "read" || slices.Contains(roles, "super_admin"), nil
}

func asReader(ctx context.Context) context.Context {
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000002", Roles: []string{"viewer"}}
	return context.WithValue(ctx, auth.ClaimsKey, claims)
}

func (fx schemaContentFixture) send(t *testing.T, ctx context.Context, method, target string, params map[string]string, body any, serve func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, reader)
	r = chiCtx(r.WithContext(ctx), params)
	rr := httptest.NewRecorder()
	serve(rr, r)
	return rr
}

func listedIDs(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var items []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &items), rr.Body.String())
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// A draft is not public. It is written as a draft, it stays out of the read
// by id and the list for a caller who cannot write the schema, and that
// caller cannot ask for drafts by filter. A caller who can write sees it.
func TestContentHandler_DraftsStayUnpublished(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{writerOnlyChecker{}}
		writer := fx.ctx
		reader := asReader(fx.ctx)
		params := map[string]string{"schema": "drafts"}

		rr := fx.send(t, writer, http.MethodPost, "/api/v1/content/drafts", params,
			map[string]any{"data": map[string]any{"title": "hidden", "_status": "draft"}}, fx.content.Create)
		id := createdID(t, rr)
		assert.Equal(t, "draft", dataOf(t, rr)["_status"])
		one := map[string]string{"schema": "drafts", "id": id}

		got := fx.send(t, reader, http.MethodGet, "/api/v1/content/drafts/"+id, one, nil, fx.content.Get)
		assert.Equal(t, http.StatusNotFound, got.Code, "a reader read a draft by id: %s", got.Body.String())
		got = fx.send(t, writer, http.MethodGet, "/api/v1/content/drafts/"+id, one, nil, fx.content.Get)
		assert.Equal(t, http.StatusOK, got.Code, "a writer cannot read the draft it saved: %s", got.Body.String())

		got = fx.send(t, reader, http.MethodGet, "/api/v1/content/drafts", params, nil, fx.content.List)
		assert.NotContains(t, listedIDs(t, got), id, "a draft is listed for a reader")

		got = fx.send(t, reader, http.MethodGet, "/api/v1/content/drafts?filters[_status]=draft", params, nil, fx.content.List)
		assert.Equal(t, http.StatusForbidden, got.Code, "a reader listed drafts by filter: %s", got.Body.String())
		got = fx.send(t, writer, http.MethodGet, "/api/v1/content/drafts?filters[_status]=draft", params, nil, fx.content.List)
		assert.Contains(t, listedIDs(t, got), id, "a writer cannot list its drafts")

		got = fx.send(t, writer, http.MethodPut, "/api/v1/content/drafts/"+id+"/publish", one, nil, fx.content.Publish)
		require.Equal(t, http.StatusNoContent, got.Code, got.Body.String())
		got = fx.send(t, reader, http.MethodGet, "/api/v1/content/drafts/"+id, one, nil, fx.content.Get)
		assert.Equal(t, http.StatusOK, got.Code, "a published entry is hidden: %s", got.Body.String())
		got = fx.send(t, reader, http.MethodGet, "/api/v1/content/drafts?filters[_status]=published", params, nil, fx.content.List)
		assert.Contains(t, listedIDs(t, got), id, "a reader cannot filter on published")

		got = fx.send(t, writer, http.MethodPut, "/api/v1/content/drafts/"+id+"/unpublish", one, nil, fx.content.Unpublish)
		require.Equal(t, http.StatusNoContent, got.Code, got.Body.String())
		got = fx.send(t, reader, http.MethodGet, "/api/v1/content/drafts/"+id, one, nil, fx.content.Get)
		assert.Equal(t, http.StatusNotFound, got.Code, "an unpublished entry is still readable: %s", got.Body.String())
	})
}
