package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

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
