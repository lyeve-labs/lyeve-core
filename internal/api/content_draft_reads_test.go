package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/auth"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// editorChecker lets every role read and lets editor and super_admin write,
// the shape of an install where the public reads what its editors maintain.
type editorChecker struct{}

func (editorChecker) Allowed(_ context.Context, roles []string, _ string, action string) (bool, error) {
	return action == "read" || slices.Contains(roles, "editor") || slices.Contains(roles, "super_admin"), nil
}

func asEditor(ctx context.Context) context.Context {
	claims := &auth.Claims{UserID: "00000000-0000-0000-0000-000000000003", Roles: []string{"editor"}}
	return context.WithValue(ctx, auth.ClaimsKey, claims)
}

// asKey is an API key holding the editor role with the given scopes.
func asKey(scopes ...string) context.Context {
	ctx := tenant.WithID(context.Background(), "t1")
	claims := &core.AuthClaims{UserID: "key-1", APIKeyID: "key-1", Roles: []string{"editor"}, Scopes: scopes, IsAPIKey: true}
	return context.WithValue(ctx, core.ClaimsKey, claims)
}

// draftFixture creates one row of schema in the given status through the
// public route, as a super admin, and returns its id.
func (fx schemaContentFixture) createStatus(t *testing.T, schema, status string, data map[string]any) string {
	t.Helper()
	data["_status"] = status
	rr := fx.send(t, fx.ctx, http.MethodPost, "/api/v1/content/"+schema, map[string]string{"schema": schema},
		map[string]any{"data": data}, fx.content.Create)
	return createdID(t, rr)
}

func cursorIDs(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	ids := make([]string, 0, len(out.Data))
	for _, it := range out.Data {
		ids = append(ids, it.ID)
	}
	return ids
}

// The cursor list served every row of the table: drafts, and rows already
// soft deleted. A reader sees what the offset list shows, and a caller who
// may read drafts sees them, never a deleted row.
func TestContentHandler_CursorHidesDraftsAndDeletedRows(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		pub := fx.createStatus(t, "drafts", "published", map[string]any{"title": "pub"})
		draft := fx.createStatus(t, "drafts", "draft", map[string]any{"title": "draft"})
		gone := fx.createStatus(t, "drafts", "published", map[string]any{"title": "gone"})
		rr := fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/drafts/"+gone,
			map[string]string{"schema": "drafts", "id": gone}, nil, fx.content.Delete)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		params := map[string]string{"schema": "drafts"}
		viewer := cursorIDs(t, fx.send(t, asReader(fx.ctx), http.MethodGet, "/api/v1/content/drafts/cursor?limit=50", params, nil, fx.content.ListCursor))
		assert.ElementsMatch(t, []string{pub}, viewer)

		editor := cursorIDs(t, fx.send(t, asEditor(fx.ctx), http.MethodGet, "/api/v1/content/drafts/cursor?limit=50", params, nil, fx.content.ListCursor))
		assert.ElementsMatch(t, []string{pub, draft}, editor)
	})
}

// A related row in draft is not listed for a reader on the relation route,
// and the total counts only what is served. A draft parent reads as absent.
func TestContentHandler_RelationRouteHidesDrafts(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"essays"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		live := fx.createStatus(t, "topics", "published", map[string]any{"name": "live"})
		hidden := fx.createStatus(t, "topics", "draft", map[string]any{"name": "hidden"})
		essay := fx.createStatus(t, "essays", "published", map[string]any{"title": "e"})
		draftEssay := fx.createStatus(t, "essays", "draft", map[string]any{"title": "d"})
		for _, id := range []string{essay, draftEssay} {
			rr := fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/essays/"+id+"/relations/topics",
				map[string]string{"schema": "essays", "id": id, "field": "topics"},
				map[string]any{"ids": []string{live, hidden}}, fx.content.SetRelations)
			require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
		}

		list := func(ctx context.Context, id string) *httptest.ResponseRecorder {
			return fx.send(t, ctx, http.MethodGet, "/api/v1/content/essays/"+id+"/relations/topics",
				map[string]string{"schema": "essays", "id": id, "field": "topics"}, nil, fx.content.ListRelations)
		}
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Total int `json:"total"`
		}
		rr := list(asReader(fx.ctx), essay)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
		require.Len(t, page.Data, 1, rr.Body.String())
		assert.Equal(t, live, page.Data[0].ID)
		assert.Equal(t, 1, page.Total)

		rr = list(asEditor(fx.ctx), essay)
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &page))
		assert.Len(t, page.Data, 2, rr.Body.String())
		assert.Equal(t, 2, page.Total)

		assert.Equal(t, http.StatusNotFound, list(asReader(fx.ctx), draftEssay).Code, "a draft's relations were listed")
	})
}

// A published row pointing at a draft serves the relation as null to a
// reader and whole to an editor, and a many_to_many leaves the draft out.
func TestContentHandler_PopulateHidesDrafts(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"essays"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		author := fx.createStatus(t, "authors", "draft", map[string]any{"name": "secret"})
		live := fx.createStatus(t, "topics", "published", map[string]any{"name": "live"})
		hidden := fx.createStatus(t, "topics", "draft", map[string]any{"name": "hidden"})
		essay := fx.createStatus(t, "essays", "published", map[string]any{"title": "e", "author": author})
		rr := fx.send(t, fx.ctx, http.MethodPut, "/api/v1/content/essays/"+essay+"/relations/topics",
			map[string]string{"schema": "essays", "id": essay, "field": "topics"},
			map[string]any{"ids": []string{live, hidden}}, fx.content.SetRelations)
		require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())

		get := func(ctx context.Context) map[string]any {
			rr := fx.send(t, ctx, http.MethodGet, "/api/v1/content/essays/"+essay+"?populate=author,topics",
				map[string]string{"schema": "essays", "id": essay}, nil, fx.content.Get)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			return dataOf(t, rr)
		}

		viewer := get(asReader(fx.ctx))
		assert.Nil(t, viewer["author"], "a draft author was populated for a reader")
		assert.NotContains(t, mustJSON(t, viewer), "secret")
		topics, _ := viewer["topics"].([]any)
		assert.Len(t, topics, 1, "a draft topic was populated for a reader")
		assert.NotContains(t, mustJSON(t, viewer), "hidden")

		editor := get(asEditor(fx.ctx))
		populated, _ := editor["author"].(map[string]any)
		require.NotNil(t, populated, "an editor lost the draft author")
		assert.Equal(t, "secret", populated["name"])
		topics, _ = editor["topics"].([]any)
		assert.Len(t, topics, 2)
	})
}

// fakeRevisions serves a fixed history for any record.
type fakeRevisions struct {
	core.RecordRevisionStore
	revs []core.RecordRevision
}

func (f fakeRevisions) ListRecordRevisions(context.Context, string, uuid.UUID) ([]core.RecordRevision, error) {
	out := make([]core.RecordRevision, len(f.revs))
	copy(out, f.revs)
	return out, nil
}

type fakeRevisionProvider struct{ store core.RecordRevisionStore }

func (p fakeRevisionProvider) RecordRevisionStore() core.RecordRevisionStore { return p.store }

// Revisions keep snapshots of every state a row passed through, drafts
// included. A reader is served the published ones, and the history of a row
// that is a draft now reads as absent, as the row does.
func TestContentHandler_RevisionsHideDrafts(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		fx.content.revisions = fakeRevisionProvider{fakeRevisions{revs: []core.RecordRevision{
			{ID: uuid.New(), Data: map[string]any{"title": "early draft", "_status": "draft"}},
			{ID: uuid.New(), Data: map[string]any{"title": "as published", "_status": "published"}},
		}}}
		pub := fx.createStatus(t, "drafts", "published", map[string]any{"title": "pub"})
		draft := fx.createStatus(t, "drafts", "draft", map[string]any{"title": "draft"})

		list := func(ctx context.Context, id string) *httptest.ResponseRecorder {
			return fx.send(t, ctx, http.MethodGet, "/api/v1/content/drafts/"+id+"/revisions",
				map[string]string{"schema": "drafts", "id": id}, nil, fx.content.ListRevisions)
		}
		rr := list(asReader(fx.ctx), pub)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), "early draft")
		assert.Contains(t, rr.Body.String(), "as published")

		assert.Equal(t, http.StatusNotFound, list(asReader(fx.ctx), draft).Code)

		rr = list(asEditor(fx.ctx), draft)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "early draft")
	})
}

// A stream sends a change to a draft only to a caller who may read drafts.
func TestContentHandler_StreamHidesDrafts(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		viewer := openStreamAs(t, fx.content, asReader(fx.ctx), "drafts")
		editor := openStreamAs(t, fx.content, asEditor(fx.ctx), "drafts")

		fx.createStatus(t, "drafts", "draft", map[string]any{"title": "unreleased"})
		fx.createStatus(t, "drafts", "published", map[string]any{"title": "released"})

		require.Eventually(t, func() bool { return len(editor.events(t)) == 2 }, 5*time.Second, 10*time.Millisecond, editor.String())
		require.Eventually(t, func() bool { return len(viewer.events(t)) >= 1 }, 5*time.Second, 10*time.Millisecond, viewer.String())
		// The draft was written first, so had it been sent it would be ahead
		// of the published row in the buffer.
		assert.Len(t, viewer.events(t), 1, viewer.String())
		assert.NotContains(t, viewer.String(), "unreleased")
	})
}

// A response that can carry drafts is the caller's alone. Served public with
// a max-age, a shared cache keyed on anything but the session would hand an
// editor's drafts to the next reader.
func TestContentHandler_DraftReadersAreNotCachedPublicly(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		id := fx.createStatus(t, "drafts", "draft", map[string]any{"title": "draft"})
		cached := apimw.CacheWithPolicy(apimw.DefaultContentPolicy)
		serve := func(ctx context.Context, target string, params map[string]string, h http.HandlerFunc) http.Header {
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r = chiCtx(r.WithContext(ctx), params)
			rr := httptest.NewRecorder()
			cached(h).ServeHTTP(rr, r)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			return rr.Header()
		}
		one := map[string]string{"schema": "drafts", "id": id}
		all := map[string]string{"schema": "drafts"}

		for name, h := range map[string]http.Header{
			"get":    serve(asEditor(fx.ctx), "/api/v1/content/drafts/"+id, one, fx.content.Get),
			"list":   serve(asEditor(fx.ctx), "/api/v1/content/drafts?filters[_status]=draft", all, fx.content.List),
			"cursor": serve(asEditor(fx.ctx), "/api/v1/content/drafts/cursor", all, fx.content.ListCursor),
		} {
			cc := h.Get("Cache-Control")
			assert.Contains(t, cc, "private", "%s: %s", name, cc)
			assert.Contains(t, cc, "no-store", "%s: %s", name, cc)
			assert.NotContains(t, cc, "public", "%s: %s", name, cc)
			assert.Empty(t, h.Get("X-Cache-Private"), "%s leaked the internal signal", name)
		}

		reader := serve(asReader(fx.ctx), "/api/v1/content/drafts", all, fx.content.List)
		assert.Contains(t, reader.Get("Cache-Control"), "public", "a reader's published list stays cacheable")
	})
}

// An API key scoped to reading content is a read credential whatever its
// roles, so it sees what the public sees. A write scope makes it an editor's
// key.
func TestContentHandler_ReadScopedKeyCannotReadDrafts(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"drafts"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		id := fx.createStatus(t, "drafts", "draft", map[string]any{"title": "draft"})
		one := map[string]string{"schema": "drafts", "id": id}
		get := func(ctx context.Context) int {
			return fx.send(t, ctx, http.MethodGet, "/api/v1/content/drafts/"+id, one, nil, fx.content.Get).Code
		}
		assert.Equal(t, http.StatusNotFound, get(asKey("content:read")))
		assert.Equal(t, http.StatusNotFound, get(asKey("*:read")))
		assert.Equal(t, http.StatusOK, get(asKey("content:read", "content:write")))
		assert.Equal(t, http.StatusOK, get(asEditor(fx.ctx)))
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
