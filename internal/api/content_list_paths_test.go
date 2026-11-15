package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// pathRows seeds the related set in tenant t1. P1 is Ann's, tagged go, with
// an older comment "old" rated 1 and a newer "new" rated 5. P2 is Bob's,
// tagged rust, with one comment "only" rated 3. P3 has no author, tag or
// comment. P4 is Dana's, a draft author. P5 is Tess's, an author in the
// trash. P6 points at an author and a tag of tenant t2, each named like one
// of t1's. Ann and Dana work at Acme, in France, and Bob at Globex.
type pathRows struct {
	byID map[string]string
	ids  map[string]string
}

func seedPathRows(t *testing.T, fx schemaContentFixture) pathRows {
	t.Helper()
	rows := pathRows{byID: map[string]string{}, ids: map[string]string{}}
	create := func(ctx context.Context, schema string, data map[string]any) string {
		rr := fx.send(t, ctx, http.MethodPost, "/api/v1/content/"+schema, map[string]string{"schema": schema},
			map[string]any{"data": data}, fx.content.Create)
		return createdID(t, rr)
	}
	france := create(fx.ctx, "rf_countries", map[string]any{"name": "France"})
	rows.ids["acme"] = create(fx.ctx, "rf_companies", map[string]any{"name": "Acme", "country_id": france})
	globex := create(fx.ctx, "rf_companies", map[string]any{"name": "Globex"})
	ann := create(fx.ctx, "rf_authors", map[string]any{"_status": "published", "name": "Ann", "secret": "s-ann", "company_id": rows.ids["acme"]})
	bob := create(fx.ctx, "rf_authors", map[string]any{"_status": "published", "name": "Bob", "company_id": globex})
	dana := create(fx.ctx, "rf_authors", map[string]any{"_status": "draft", "name": "Dana", "company_id": rows.ids["acme"]})
	tess := create(fx.ctx, "rf_authors", map[string]any{"_status": "published", "name": "Tess"})
	del := fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/rf_authors/"+tess,
		map[string]string{"schema": "rf_authors", "id": tess}, nil, fx.content.Delete)
	require.Equal(t, http.StatusNoContent, del.Code, del.Body.String())
	goTag := create(fx.ctx, "rf_tags", map[string]any{"slug": "go"})
	rustTag := create(fx.ctx, "rf_tags", map[string]any{"slug": "rust"})

	other := asSuperAdmin(tenant.WithID(context.Background(), "t2"))
	otherAnn := create(other, "rf_authors", map[string]any{"_status": "published", "name": "Ann"})
	otherGo := create(other, "rf_tags", map[string]any{"slug": "go"})

	post := func(name string, data map[string]any) string {
		data["title"] = name
		id := create(fx.ctx, "rf_posts", data)
		rows.byID[id] = name
		rows.ids[name] = id
		return id
	}
	p1 := post("P1", map[string]any{"author_id": ann})
	p2 := post("P2", map[string]any{"author_id": bob})
	post("P3", map[string]any{})
	post("P4", map[string]any{"author_id": dana})
	post("P5", map[string]any{"author_id": tess})
	p6 := post("P6", map[string]any{})
	fx.linkAndList(t, "rf_posts", p1, "tags", []string{goTag})
	fx.linkAndList(t, "rf_posts", p2, "tags", []string{rustTag})

	// The write path refuses another tenant's row, so the links that
	// cross tenants are written straight to the tables.
	_, err := fx.pool.Exec(fx.ctx, "UPDATE "+core.TableName("rf_posts")+" SET author_id = $1 WHERE id = $2", otherAnn, p6)
	require.NoError(t, err)
	_, err = fx.pool.Exec(fx.ctx, "INSERT INTO "+core.PivotTableName("rf_posts", "rf_tags")+" (rf_posts_id, rf_tags_id, tenant_id) VALUES ($1, $2, $3)", p6, otherGo, "t1")
	require.NoError(t, err)

	comment := func(postID, body string, rating int, at string) {
		id := create(fx.ctx, "rf_comments", map[string]any{"body": body, "rating": rating, "post_id": postID})
		when, err := time.Parse(time.RFC3339, at)
		require.NoError(t, err)
		_, err = fx.pool.Exec(fx.ctx, "UPDATE "+core.TableName("rf_comments")+" SET created_at = $1 WHERE id = $2", when, id)
		require.NoError(t, err)
	}
	comment(p1, "old", 1, "2026-01-01T10:00:00Z")
	comment(p1, "new", 5, "2026-01-02T10:00:00Z")
	comment(p2, "only", 3, "2026-01-01T10:00:00Z")
	return rows
}

// names answers the seeded name of each post rr lists.
func (rows pathRows) names(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	var out []string
	for _, e := range listedRows(t, rr) {
		out = append(out, rows.byID[e.ID])
	}
	return out
}

func (fx schemaContentFixture) listPosts(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/rf_posts?limit=50&"+query,
		map[string]string{"schema": "rf_posts"}, nil, fx.content.List)
}

// A key may name a field of a related entry: through a belongs_to, a
// many_to_many's pivot, a has_many and a has_one, which reads its newest
// entry alone, and through two relations. A positive operator matches an
// entry with a related entry for which it holds. ne, ncontains and nin match
// an entry with none for which eq, contains or in holds, so an entry with no
// related entry matches them, and empty=true is the negation of
// empty=false. A related entry in the trash or in another tenant never
// counts. The caller is a super admin, so a draft author counts.
func TestContentList_FiltersOnARelatedEntrysFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"related"}, func(t *testing.T, fx schemaContentFixture) {
		rows := seedPathRows(t, fx)
		for query, want := range map[string][]string{
			"filters[author.name]=Bob":                                    {"P2"},
			"filters[author.name][eq]=Ann":                                {"P1"},
			"filters[author.name][contains]=AN":                           {"P1", "P4"},
			"filters[author.name][eq]=Tess":                               nil,
			"filters[author.name][ne]=Ann":                                {"P2", "P3", "P4", "P5", "P6"},
			"filters[author.name][ncontains]=n":                           {"P3", "P5", "P6", "P2"},
			"filters[author.name][nin]=Ann&filters[author.name][nin]=Bob": {"P3", "P4", "P5", "P6"},
			"filters[author.secret][empty]=false":                         {"P1"},
			"filters[author.secret][empty]=true":                          {"P2", "P3", "P4", "P5", "P6"},
			"filters[author.created_at][gt]=2000-01-01T00:00:00Z":         {"P1", "P2", "P4"},
			"filters[author.id][eq]=" + rows.ids["P1"]:                    nil,
			"filters[tags.slug][eq]=go":                                   {"P1"},
			"filters[tags.slug][in]=go&filters[tags.slug][in]=rust":       {"P1", "P2"},
			"filters[tags.slug][ne]=go":                                   {"P2", "P3", "P4", "P5", "P6"},
			"filters[tags.slug][nin]=go":                                  {"P2", "P3", "P4", "P5", "P6"},
			"filters[tags.slug][empty]=false":                             {"P1", "P2"},
			"filters[tags.slug][empty]=true":                              {"P3", "P4", "P5", "P6"},
			"filters[comments.body][eq]=old":                              {"P1"},
			"filters[comments.body][ncontains]=o":                         {"P3", "P4", "P5", "P6"},
			"filters[comments.rating][gte]=3":                             {"P1", "P2"},
			"filters[comments.rating][lt]=2":                              {"P1"},
			"filters[comments.rating][gt]=4":                              {"P1"},
			"filters[lead.body][eq]=new":                                  {"P1"},
			"filters[lead.body][eq]=old":                                  nil,
			"filters[lead.body][eq]=only":                                 {"P2"},
			"filters[lead.rating][lt]=2":                                  nil,
			"filters[lead.body][ne]=new":                                  {"P2", "P3", "P4", "P5", "P6"},
			"filters[author.company.name][eq]=Acme":                       {"P1", "P4"},
			"filters[author.company.name][ne]=Acme":                       {"P2", "P3", "P5", "P6"},
			"filters[author.company][eq]=" + rows.ids["acme"]:             {"P1", "P4"},
			"filters[author.company][empty]=true":                         {"P3", "P5", "P6"},
			"filters[author.name][eq]=Ann&filters[tags.slug][eq]=go":      {"P1"},
			"filters[author.name][eq]=Ann&filters[tags.slug][eq]=rust":    nil,
			"filters[title][ne]=P1&filters[author.company.name][eq]=Acme": {"P4"},
		} {
			assert.ElementsMatch(t, want, rows.names(t, fx.listPosts(t, query)), query)
		}
	})
}

// A path past two relations, counting one it ends on, a step that is not a
// relation, a last field the related type does not declare, a many_to_many
// from a type to itself, a has_many whose key cannot be read, an operator
// the last field's type does not take and a sort answer 400 with the words
// of an unknown key, so no refusal says why.
func TestContentList_RefusesAPathItCannotRead(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"related"}, func(t *testing.T, fx schemaContentFixture) {
		rows := seedPathRows(t, fx)
		unknown := fx.listPosts(t, "filters[nosuch][ne]=x")
		require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
		for _, q := range []string{
			"filters[author.company.country.name][eq]=France",
			"filters[author.company.country][eq]=" + rows.ids["acme"],
			"filters[author.company.country][empty]=true",
			"filters[title.name][eq]=x", "filters[author.nosuch][eq]=x", "filters[author.company_id][eq]=" + rows.ids["acme"],
			"filters[author._status][eq]=draft", "filters[author.deleted_at][empty]=false",
			"filters[related.title][eq]=P1", "filters[sources.name][eq]=Acme",
			"filters[author.name][gt]=a", "filters[author.name][like]=a", "filters[comments.rating][contains]=1",
			"filters[author.][eq]=x", "filters[.name][eq]=x", "filters[author.tags][eq]=x",
			"sort=author.created_at", "sort=-comments.rating",
		} {
			rr := fx.listPosts(t, q)
			require.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
			assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
		}
	})
}

// A has_one or has_many reads its key by the rule a populated one follows: a
// key held as text, the column named for the source type, is read on every
// dialect, and one the rule cannot read answers 400 like an unknown key.
func TestContentList_PathReadsTheInverseKeyRule(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"inverse"}, func(t *testing.T, fx schemaContentFixture) {
		post := createdID(t, fx.create(t, "iv_posts", map[string]any{"title": "a"}))
		createdID(t, fx.create(t, "iv_posts", map[string]any{"title": "b"}))
		createdID(t, fx.create(t, "iv_legacy", map[string]any{"body": "kept", "iv_posts_id": post}))
		createdID(t, fx.create(t, "iv_replies", map[string]any{"body": "r", "reply_to_id": post}))
		list := func(q string) *httptest.ResponseRecorder {
			return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/iv_posts?"+q,
				map[string]string{"schema": "iv_posts"}, nil, fx.content.List)
		}
		for _, q := range []string{"filters[legacy.body][eq]=kept", "filters[replies.body][eq]=r"} {
			got := listedRows(t, list(q))
			require.Len(t, got, 1, q)
			assert.Equal(t, post, got[0].ID, q)
		}
		unknown := list("filters[nosuch][ne]=x")
		for _, q := range []string{
			"filters[ambiguous.body][eq]=r", "filters[wrongkey.body][eq]=r",
			"filters[orphans.body][eq]=r", "filters[lone.body][empty]=true",
		} {
			rr := list(q)
			require.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
			assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
		}
	})
}

// A related entry counts only when the caller could be served it: a draft
// author counts for a caller who may read the authors' drafts and for no
// one else, and that answer is marked private. A path into a type the
// caller may not read, onto a field its mask hides, or through a step whose
// key its mask hides answers 400 like an unknown key, while a filter on the
// listed type's own hidden field keeps its 403.
func TestContentList_PathsHoldTheCallersReadRights(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"related"}, func(t *testing.T, fx schemaContentFixture) {
		rows := seedPathRows(t, fx)
		readable := []string{"rf_posts", "rf_authors", "rf_tags", "rf_comments"}
		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{
				"reader":  readable,
				"drafter": append([]string{"rf_companies", "rf_countries"}, readable...),
			},
			updates: map[string][]string{"drafter": {"rf_authors"}},
			mask:    map[string][]string{"rf_authors": {"secret"}},
		}}
		scopes := []string{"content:read", "content:write"}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader":  {Roles: []string{"reader"}, Scopes: scopes},
			"drafter": {Roles: []string{"drafter"}, Scopes: scopes},
		})
		get := func(key, query string) *httptest.ResponseRecorder {
			return call(t, r, http.MethodGet, key, "/api/v1/content/rf_posts?limit=50&"+query, nil)
		}

		assert.Empty(t, rows.names(t, get("reader", "filters[author.name][eq]=Dana")), "a draft author counted for a reader")
		assert.ElementsMatch(t, []string{"P2", "P3", "P4", "P5", "P6"}, rows.names(t, get("reader", "filters[author.name][ne]=Ann")))
		drafts := get("drafter", "filters[author.name][eq]=Dana")
		assert.ElementsMatch(t, []string{"P4"}, rows.names(t, drafts))
		assert.Equal(t, "1", drafts.Header().Get(apimw.PrivateResponseHeader), "an answer that counted drafts was not marked private")
		assert.ElementsMatch(t, []string{"P1", "P4"}, rows.names(t, get("drafter", "filters[author.company.name][eq]=Acme")))

		refusedLike := func(key string, queries ...string) {
			t.Helper()
			unknown := get(key, "filters[nosuch][ne]=x")
			require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
			for _, q := range queries {
				rr := get(key, q)
				require.Equal(t, http.StatusBadRequest, rr.Code, "%s %s: %s", key, q, rr.Body.String())
				assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
			}
		}
		refusedLike("reader", "filters[author.company.name][eq]=Acme", "filters[author.secret][eq]=s-ann", "filters[author.secret][empty]=false")
		refusedLike("drafter", "filters[author.secret][contains]=s", "filters[author.company.country.name][eq]=France")

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"rf_posts", "rf_authors", "rf_tags", "rf_comments", "rf_companies"}},
			mask:  map[string][]string{"rf_posts": {"author"}, "rf_comments": {"post"}},
		}}
		refusedLike("reader", "filters[author.name][eq]=Ann", "filters[author.company.name][eq]=Acme", "filters[comments.body][eq]=old", "filters[lead.body][empty]=false")
		assert.Equal(t, http.StatusForbidden, get("reader", "filters[author_id]="+rows.ids["P1"]).Code, "the listed type's hidden field")
		assert.ElementsMatch(t, []string{"P1"}, rows.names(t, get("reader", "filters[tags.slug][eq]=go")))
	})
}
