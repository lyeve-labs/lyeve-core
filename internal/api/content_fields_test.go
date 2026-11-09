package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/hooks"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestFieldNames_SplitsAndRefusesAnEmptyName(t *testing.T) {
	names, ok := fieldNames("title, body")
	require.True(t, ok)
	assert.Equal(t, []string{"title", "body"}, names)

	for _, v := range []string{"", ",", "title,", "title,,body", " "} {
		_, ok := fieldNames(v)
		assert.False(t, ok, "%q", v)
	}
}

func TestDeliveryFieldNames_RefusesASecondURLForOneBody(t *testing.T) {
	names, ok := deliveryFieldNames("title,body")
	require.True(t, ok)
	assert.Equal(t, []string{"title", "body"}, names)

	for _, v := range []string{"", ",", "title,", " title", "title ", "title,title", "id,title", "title,created_at", "updated_at"} {
		_, ok := deliveryFieldNames(v)
		assert.False(t, ok, "%q", v)
	}
}

func TestResolveFields_NamesAgainstTheType(t *testing.T) {
	sc := &core.Schema{
		Name:             "posts",
		WithDraftPublish: true,
		Fields: []core.SchemaField{
			{Name: "id", FieldType: "uid", System: true},
			{Name: "title", FieldType: "text"},
			{Name: "body", FieldType: "text"},
			{Name: "author", FieldType: "relation", RelationTo: "people", RelationType: core.RelBelongsTo},
			{Name: "editor", FieldType: "relation", RelationTo: "people", RelationType: core.RelBelongsTo, RelationFKName: "edited_by"},
			{Name: "tags", FieldType: "relation", RelationTo: "tags", RelationType: core.RelManyToMany},
		},
	}
	hidden := core.HiddenFields{"body": {}, "editor": {}}.WithRelations(sc.Fields)

	for _, tc := range []struct {
		names []string
		want  []string
	}{
		{[]string{"title"}, []string{"title"}},
		{[]string{"id", "created_at", "updated_at"}, []string{}},
		{[]string{"author"}, []string{"author", "author_id"}},
		{[]string{"author_id"}, []string{"author_id"}},
		{[]string{"tags", "_status"}, []string{"tags", "_status"}},
		{[]string{"title", "title"}, []string{"title"}},
	} {
		got, ok := resolveFields(sc, hidden, tc.names)
		require.True(t, ok, "%v", tc.names)
		require.NotNil(t, got, "%v", tc.names)
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, tc.want, keys, "%v", tc.names)
	}

	for _, names := range [][]string{{"nosuch"}, {"body"}, {"editor"}, {"edited_by"}, {"_locale"}, {"deleted_at"}, {"title", "nosuch"}} {
		_, ok := resolveFields(sc, hidden, names)
		assert.False(t, ok, "%v", names)
	}

	got, ok := resolveFields(sc, hidden, nil)
	assert.True(t, ok)
	assert.Nil(t, got, "no names keep every key")
}

func TestContentFields_ApplyCopiesAndLeavesTheInputWhole(t *testing.T) {
	data := map[string]any{"title": "t", "body": "b"}
	assert.Equal(t, data, contentFields(nil).apply(data))

	got := contentFields{"title": {}, "missing": {}}.apply(data)
	assert.Equal(t, map[string]any{"title": "t"}, got)
	assert.Equal(t, map[string]any{"title": "t", "body": "b"}, data)
	assert.Equal(t, map[string]any{}, contentFields{}.apply(data))
}

// ?fields= narrows the data of every entry a read serves, by list, by cursor
// and by id, and the entry keeps its id and timestamps.
func TestContentHandler_FieldsNarrowEachRead(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{"title": "first", "body": "hello"}))
		fx.content.perms = fakeProvider{ruleChecker{reads: map[string][]string{"reader": {"notes"}}}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		for _, path := range []string{"/api/v1/content/notes", "/api/v1/content/notes/cursor", "/api/v1/content/notes/" + id} {
			rr := call(t, r, http.MethodGet, "reader", path+"?fields=title,id", nil)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", path, rr.Body.String())
			entries := servedEntries(t, rr)
			require.Len(t, entries, 1, path)
			assert.Equal(t, map[string]any{"title": "first"}, entries[0]["data"], path)
			assert.Equal(t, id, entries[0]["id"], path)
			assert.Contains(t, entries[0], "created_at", path)
			assert.Contains(t, entries[0], "updated_at", path)

			rr = call(t, r, http.MethodGet, "reader", path+"?fields=id", nil)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", path, rr.Body.String())
			assert.Equal(t, map[string]any{}, servedEntries(t, rr)[0]["data"], path)

			rr = call(t, r, http.MethodGet, "reader", path+"?fields=", nil)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", path, rr.Body.String())
			data, _ := servedEntries(t, rr)[0]["data"].(map[string]any)
			assert.Equal(t, "first", data["title"], "an empty fields keeps every key: %s", path)
			assert.Equal(t, "hello", data["body"], "an empty fields keeps every key: %s", path)
		}
	})
}

// A field the caller's mask hides and a field the type does not have get one
// answer, so the list cannot tell them apart.
func TestContentHandler_FieldsNamingAHiddenFieldAnswersLikeAnUnknownOne(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{"title": "secret title", "body": "hello"}))
		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"notes"}},
			mask:  map[string][]string{"notes": {"title"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		for _, path := range []string{"/api/v1/content/notes", "/api/v1/content/notes/" + id} {
			unknown := call(t, r, http.MethodGet, "reader", path+"?fields=nosuch", nil)
			require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
			for _, q := range []string{"title", "body,title", "title,"} {
				rr := call(t, r, http.MethodGet, "reader", path+"?fields="+q, nil)
				assert.Equal(t, unknown.Code, rr.Code, q)
				assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
				assert.NotContains(t, rr.Body.String(), "secret title")
			}

			rr := call(t, r, http.MethodGet, "reader", path+"?fields=body", nil)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			assert.Equal(t, map[string]any{"body": "hello"}, servedEntries(t, rr)[0]["data"])
		}
	})
}

// A relation field's name keeps both the column it is stored in and the
// record populate resolves into it.
func TestContentHandler_FieldsKeepARelationsColumnAndRecord(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"surveys"}, func(t *testing.T, fx schemaContentFixture) {
		surveyID := fx.createSurvey(t)
		id := createdID(t, fx.create(t, "survey_responses", map[string]any{
			"survey": surveyID, "data": map[string]any{"answer": "by reading"},
		}))
		fx.content.perms = fakeProvider{ruleChecker{reads: map[string][]string{"reader": {"surveys", "survey_responses"}}}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		rr := call(t, r, http.MethodGet, "reader", "/api/v1/content/survey_responses/"+id+"?fields=survey&populate=survey", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		data := dataOf(t, rr)
		assert.ElementsMatch(t, []string{"survey", "survey_id"}, keysOf(data))
		survey, _ := data["survey"].(map[string]any)
		require.NotNil(t, survey, rr.Body.String())
		assert.Equal(t, "Onboarding", survey["name"])

		rr = call(t, r, http.MethodGet, "reader", "/api/v1/content/survey_responses/"+id+"?fields=status&populate=survey", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.Equal(t, map[string]any{"status": "new"}, dataOf(t, rr))
	})
}

// Delivery takes fields on a list and on a read by id, and refuses a private
// field and a relation into a type it does not serve with the answer a field
// the type does not have gets.
func TestDelivery_FieldsNarrowEntriesAndRefuseHiddenOnesAlike(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		vault := rg.post(t, "t1", "dl_vaults", "", map[string]any{"code": "v-code"})
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "body": "b", "secret": "hush", "vault": vault})

		rows := deliveredRows(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?fields=title", nil))
		require.Len(t, rows, 1)
		assert.Equal(t, map[string]any{"title": "t"}, rows[0])
		one := rg.get(t, deliveryHostA, deliveryPostsPath+"/"+id+"?fields=body", nil)
		require.Equal(t, http.StatusOK, one.Code, one.Body.String())
		assert.Equal(t, map[string]any{"body": "b"}, dataOf(t, one))

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			unknown := rg.get(t, deliveryHostA, path+"?fields=nosuch", nil)
			require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
			for _, q := range []string{"secret", "vault", "vault_id", "title,", "", "title,title", "%20title", "id,title"} {
				rr := rg.get(t, deliveryHostA, path+"?fields="+q, nil)
				assert.Equal(t, unknown.Code, rr.Code, q)
				assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
				assert.NotContains(t, rr.Body.String(), "hush")
			}
		}
	})
}

// The selection is made after the after-response hooks, so a hook sees the
// same data it sees on a read with no fields.
func TestContentHandler_AfterResponseHookSeesEveryField(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"notes"}, func(t *testing.T, fx schemaContentFixture) {
		id := createdID(t, fx.create(t, "notes", map[string]any{"title": "first", "body": "hello"}))
		var mu sync.Mutex
		var seen []map[string]any
		fx.content.hooks.Register("notes", hooks.AfterResponse, func(_ context.Context, e hooks.Event) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, e.Data)
			return nil
		})
		fx.content.perms = fakeProvider{ruleChecker{reads: map[string][]string{"reader": {"notes"}}}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})

		paths := []string{"/api/v1/content/notes", "/api/v1/content/notes/cursor", "/api/v1/content/notes/" + id}
		for _, path := range paths {
			rr := call(t, r, http.MethodGet, "reader", path+"?fields=title", nil)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", path, rr.Body.String())
		}
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, seen, len(paths))
		for _, data := range seen {
			assert.Equal(t, "first", data["title"])
			assert.Equal(t, "hello", data["body"])
		}
	})
}

func TestDelivery_AfterResponseHookSeesEveryServedField(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		id := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "t", "body": "b"})
		var mu sync.Mutex
		var seen []map[string]any
		rg.hooks.Register("dl_posts", hooks.AfterResponse, func(_ context.Context, e hooks.Event) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, e.Data)
			return nil
		})

		for _, path := range []string{deliveryPostsPath, deliveryPostsPath + "/" + id} {
			require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, path+"?fields=title", nil).Code, path)
		}
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, seen, 2)
		for _, data := range seen {
			assert.Equal(t, "t", data["title"])
			assert.Equal(t, "b", data["body"])
		}
	})
}

// servedEntries decodes a read of entries, a bare list, a cursor page or one
// entry, into each entry's JSON object.
func servedEntries(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	raw := rr.Body.Bytes()
	var list []map[string]any
	if json.Unmarshal(raw, &list) == nil {
		return list
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &page) == nil && page.Data != nil {
		return page.Data
	}
	var one map[string]any
	require.NoError(t, json.Unmarshal(raw, &one), string(raw))
	return []map[string]any{one}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
