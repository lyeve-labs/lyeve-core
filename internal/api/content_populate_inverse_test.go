package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A has_one or has_many reads the entries of its target whose key points
// back, by the key rule: the key the field names, the one belongs_to back,
// or a column named for the source type. A field no key serves is refused
// when the request names it and left out when a wildcard or a depth reaches
// it.
func TestContentHandler_PopulateInverseReadsTheKeyByTheRule(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"inverse"}, func(t *testing.T, fx schemaContentFixture) {
		post := createdID(t, fx.create(t, "iv_posts", map[string]any{"title": "p"}))
		other := createdID(t, fx.create(t, "iv_posts", map[string]any{"title": "q"}))
		c1 := createdID(t, fx.create(t, "iv_comments", map[string]any{"body": "c1", "post": post}))
		c2 := createdID(t, fx.create(t, "iv_comments", map[string]any{"body": "c2", "post": post}))
		createdID(t, fx.create(t, "iv_comments", map[string]any{"body": "c3", "post": other}))
		createdID(t, fx.create(t, "iv_replies", map[string]any{"body": "r1", "post": post, "reply_to": other}))
		r2 := createdID(t, fx.create(t, "iv_replies", map[string]any{"body": "r2", "post": other, "reply_to": post}))
		l1 := createdID(t, fx.create(t, "iv_legacy", map[string]any{"body": "l1", "iv_posts_id": post}))
		createdID(t, fx.create(t, "iv_orphans", map[string]any{"body": "o1"}))

		get := func(t *testing.T, query string) (int, map[string]any, string) {
			t.Helper()
			rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/iv_posts/"+post+"?"+query,
				map[string]string{"schema": "iv_posts", "id": post}, nil, fx.content.Get)
			var out struct {
				Data map[string]any `json:"data"`
			}
			if rr.Code == http.StatusOK {
				require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
			}
			return rr.Code, out.Data, rr.Body.String()
		}
		ids := func(t *testing.T, v any) []string {
			t.Helper()
			list, ok := v.([]any)
			require.True(t, ok, "%T is no list", v)
			out := []string{}
			for _, item := range list {
				out = append(out, item.(map[string]any)["id"].(string))
			}
			slices.Sort(out)
			return out
		}
		sorted := func(s ...string) []string {
			slices.Sort(s)
			return s
		}

		code, data, body := get(t, "populate=comments,lead,replies,legacy")
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, sorted(c1, c2), ids(t, data["comments"]), "the one belongs_to back")
		lead, ok := data["lead"].(map[string]any)
		require.True(t, ok, "lead: %v", data["lead"])
		assert.Contains(t, []string{c1, c2}, lead["id"], "a has_one reads the same key")
		assert.Equal(t, []string{r2}, ids(t, data["replies"]), "the key the field names")
		assert.Equal(t, []string{l1}, ids(t, data["legacy"]), "a column named for the source type")

		for _, field := range []string{"ambiguous", "wrongkey", "orphans", "lone"} {
			code, _, body := get(t, "populate="+field)
			assert.Equal(t, http.StatusBadRequest, code, "%s: %s", field, body)
			assert.Contains(t, body, field, "the refusal names the field")
		}

		for _, query := range []string{"populate=*", "depth=1"} {
			code, data, body := get(t, query)
			require.Equal(t, http.StatusOK, code, "%s: %s", query, body)
			for _, field := range []string{"comments", "lead", "replies", "legacy"} {
				assert.Contains(t, data, field, "%s serves %s", query, field)
			}
			for _, field := range []string{"ambiguous", "wrongkey", "orphans", "lone"} {
				assert.NotContains(t, data, field, "%s leaves out %s", query, field)
			}
		}

		listComments := func(query string) *http.Response {
			rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/iv_comments?"+query,
				map[string]string{"schema": "iv_comments"}, nil, fx.content.List)
			return rr.Result()
		}
		named := listComments("populate=post.orphans")
		assert.Equal(t, http.StatusBadRequest, named.StatusCode, "a field named one level down")
		assert.Equal(t, http.StatusOK, listComments("populate=post.*").StatusCode, "a field a wildcard reaches one level down")
	})
}

// A has_one or has_many whose key the caller's mask hides on its target is
// left out, named or reached by a wildcard: the entries grouped under each
// record would show which record each one points at. It is left out rather
// than refused, so the answer does not say what the mask hides. A key the
// mask leaves visible is still read, and a caller who may see the key gets
// the relation.
func TestContentHandler_PopulateInverseWithAHiddenKeyIsLeftOut(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"inverse"}, func(t *testing.T, fx schemaContentFixture) {
		post := createdID(t, fx.create(t, "iv_posts", map[string]any{"title": "p"}))
		other := createdID(t, fx.create(t, "iv_posts", map[string]any{"title": "q"}))
		createdID(t, fx.create(t, "iv_comments", map[string]any{"body": "c1", "post": post}))
		createdID(t, fx.create(t, "iv_replies", map[string]any{"body": "r1", "post": other, "reply_to": post}))
		editor := asEditor(fx.ctx)
		one := map[string]string{"schema": "iv_posts", "id": post}
		read := func(t *testing.T, query string) map[string]any {
			t.Helper()
			rr := fx.send(t, editor, http.MethodGet, "/api/v1/content/iv_posts/"+post+"?"+query, one, nil, fx.content.Get)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", query, rr.Body.String())
			return dataOf(t, rr)
		}

		fx.content.perms = fakeProvider{fakeChecker{allow: true, mask: []string{"post_id"}}}
		for _, query := range []string{"populate=comments,lead,replies", "populate=*", "depth=1"} {
			got := read(t, query)
			assert.NotContains(t, got, "comments", "%s served a has_many by a hidden key", query)
			assert.NotContains(t, got, "lead", "%s served a has_one by a hidden key", query)
			assert.Contains(t, got, "replies", "%s left out a relation whose key is visible", query)
		}

		fx.content.perms = fakeProvider{fakeChecker{allow: true}}
		got := read(t, "populate=comments,lead")
		assert.Len(t, got["comments"], 1, "a caller who sees the key reads the relation")
		assert.NotNil(t, got["lead"])
	})
}

// A has_one serves the newest entry pointing back that the caller may read.
// A newer draft is not that entry for a reader without draft rights, who gets
// the published one, and an editor gets the draft.
func TestContentHandler_PopulateHasOneSkipsADraftTheCallerCannotRead(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"profiles"}, func(t *testing.T, fx schemaContentFixture) {
		fx.content.perms = fakeProvider{editorChecker{}}
		person := createdID(t, fx.create(t, "pf_people", map[string]any{"name": "p"}))
		published := fx.createStatus(t, "pf_profiles", "published", map[string]any{"bio": "published", "person": person})
		// SQL Server's clock ticks about every 15 ms, so two creates in a row
		// can share a created_at, and the id would then decide.
		time.Sleep(50 * time.Millisecond)
		draft := fx.createStatus(t, "pf_profiles", "draft", map[string]any{"bio": "draft", "person": person})

		profile := func(ctx context.Context) any {
			t.Helper()
			rr := fx.send(t, ctx, http.MethodGet, "/api/v1/content/pf_people/"+person+"?populate=profile",
				map[string]string{"schema": "pf_people", "id": person}, nil, fx.content.Get)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var out struct {
				Data map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
			got, _ := out.Data["profile"].(map[string]any)
			require.NotNil(t, got, "profile served as %v", out.Data["profile"])
			return got["id"]
		}
		assert.Equal(t, published, profile(asReader(fx.ctx)), "a reader gets the published profile")
		assert.Equal(t, draft, profile(asEditor(fx.ctx)), "an editor gets the newer draft")
	})
}
