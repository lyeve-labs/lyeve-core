package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// operatorRows seeds op_items: A and B hold a value in every field, with B's
// link and photo the empty string, C holds an empty title and nothing else,
// D is a draft and E is deleted. Neither D nor E may reach a published list
// through any operator.
type operatorRows struct {
	ids            map[string]string
	byID           map[string]string
	codeA, codeB   string
	ownerA, ownerB string
}

func seedOperatorRows(t *testing.T, fx schemaContentFixture) operatorRows {
	t.Helper()
	rows := operatorRows{
		ids:    map[string]string{},
		byID:   map[string]string{},
		codeA:  uuid.NewString(),
		codeB:  uuid.NewString(),
		ownerA: createdID(t, fx.create(t, "op_owners", map[string]any{"name": "first"})),
		ownerB: createdID(t, fx.create(t, "op_owners", map[string]any{"name": "second"})),
	}
	add := func(name, status string, data map[string]any) {
		id := fx.createStatus(t, "op_items", status, data)
		rows.ids[name] = id
		rows.byID[id] = name
	}
	add("A", "published", map[string]any{
		"title": "Alpha", "notes": "50% off_today", "mail": "a@x.io", "link": "https://a.io",
		"code": rows.codeA, "score": 1.5, "rank": 1, "price": "1.00", "live": true,
		"day": "2026-01-01", "at": "2026-01-01T10:00:00Z", "owner_id": rows.ownerA,
		"photo": "a.png", "meta": map[string]any{"k": 1}, "secret": "s1",
	})
	add("B", "published", map[string]any{
		"title": "beta", "notes": `50 percent \ back`, "mail": "b@x.io", "link": "",
		"code": rows.codeB, "score": 2, "rank": 2, "price": "2.50", "live": false,
		"day": "2026-01-02", "at": "2026-01-02T10:00:00Z", "owner_id": rows.ownerB,
		"photo": "", "secret": "s2",
	})
	add("C", "published", map[string]any{"title": "", "rank": 3})
	add("D", "draft", map[string]any{"title": "Gamma", "rank": 4})
	add("E", "published", map[string]any{"title": "Delta", "rank": 5})
	del := fx.send(t, fx.ctx, http.MethodDelete, "/api/v1/content/op_items/"+rows.ids["E"],
		map[string]string{"schema": "op_items", "id": rows.ids["E"]}, nil, fx.content.Delete)
	require.Equal(t, http.StatusNoContent, del.Code, del.Body.String())
	return rows
}

// names lists op_items as the super admin and answers the seeded name of
// each entry served.
func (rows operatorRows) names(t *testing.T, fx schemaContentFixture, query string) []string {
	t.Helper()
	rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/op_items?limit=50&"+query,
		map[string]string{"schema": "op_items"}, nil, fx.content.List)
	var out []string
	for _, e := range listedRows(t, rr) {
		out = append(out, rows.byID[e.ID])
	}
	return out
}

func (fx schemaContentFixture) listItems(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/op_items?"+query,
		map[string]string{"schema": "op_items"}, nil, fx.content.List)
}

// Each operator a field type takes selects the entries it names on every
// dialect. ne, ncontains and nin keep an entry with no value, and for text an
// entry holding the empty string, and contains matches without regard to case
// and reads %, _ and the backslash as themselves. A json field is empty when
// it holds no value, a JSON null, an empty string, an empty list or an empty
// object, and a media field when it holds no value, an empty string or an
// empty list.
func TestContentList_OperatorsByFieldType(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"operators"}, func(t *testing.T, fx schemaContentFixture) {
		rows := seedOperatorRows(t, fx)
		for query, want := range map[string][]string{
			"filters[title][eq]=Alpha":                                             {"A"},
			"filters[title][ne]=Alpha":                                             {"B", "C"},
			"filters[title][contains]=ALP":                                         {"A"},
			"filters[title][ncontains]=alp":                                        {"B", "C"},
			"filters[title][in]=Alpha&filters[title][in]=beta":                     {"A", "B"},
			"filters[title][nin]=Alpha":                                            {"B", "C"},
			"filters[title][empty]=true":                                           {"C"},
			"filters[title][empty]=false":                                          {"A", "B"},
			"filters[notes][contains]=" + url.QueryEscape("0%"):                    {"A"},
			"filters[notes][contains]=_":                                           {"A"},
			"filters[notes][contains]=%5C":                                         {"B"},
			"filters[notes][ncontains]=" + url.QueryEscape("%"):                    {"B", "C"},
			"filters[mail][contains]=" + url.QueryEscape("@X.IO"):                  {"A", "B"},
			"filters[mail][nin]=" + url.QueryEscape("a@x.io"):                      {"B", "C"},
			"filters[link][empty]=true":                                            {"B", "C"},
			"filters[link][empty]=false":                                           {"A"},
			"filters[link][ne]=" + url.QueryEscape("https://a.io"):                 {"B", "C"},
			"filters[code][ne]=" + rows.codeA:                                      {"B", "C"},
			"filters[code][in]=" + rows.codeA + "&filters[code][in]=" + rows.codeB: {"A", "B"},
			"filters[code][contains]=" + rows.codeA[:8]:                            {"A"},
			"filters[code][empty]=true":                                            {"C"},
			"filters[score][gt]=1.5":                                               {"B"},
			"filters[score][lt]=2":                                                 {"A"},
			"filters[score][ne]=2":                                                 {"A", "C"},
			"filters[score][in]=1.5&filters[score][in]=2":                          {"A", "B"},
			"filters[score][nin]=2":                                                {"A", "C"},
			"filters[score][empty]=true":                                           {"C"},
			"filters[rank][gt]=1":                                                  {"B", "C"},
			"filters[rank][lt]=2":                                                  {"A"},
			"filters[rank][nin]=1&filters[rank][nin]=2":                            {"C"},
			"filters[price][ne]=1.00":                                              {"B", "C"},
			"filters[price][in]=2.5":                                               {"B"},
			"filters[price][gt]=1":                                                 {"B"},
			"filters[price][lt]=2.50":                                              {"A"},
			"filters[live][ne]=true":                                               {"B", "C"},
			"filters[live][empty]=true":                                            {"C"},
			"filters[day][gt]=2026-01-01":                                          {"B"},
			"filters[day][lt]=2026-01-02":                                          {"A"},
			"filters[day][ne]=2026-01-01":                                          {"B", "C"},
			"filters[day][empty]=false":                                            {"A", "B"},
			"filters[at][gt]=2026-01-01T10:00:00Z":                                 {"B"},
			"filters[at][lt]=2026-01-02T10:00:00Z":                                 {"A"},
			"filters[at][ne]=2026-01-01T10:00:00Z":                                 {"B", "C"},
			"filters[created_at][gt]=2000-01-01T00:00:00Z":                         {"A", "B", "C"},
			"filters[created_at][ne]=2000-01-01T00:00:00Z":                         {"A", "B", "C"},
			"filters[created_at][empty]=true":                                      nil,
			"filters[owner_id][ne]=" + rows.ownerA:                                 {"B", "C"},
			"filters[owner_id][in]=" + rows.ownerA + "&filters[owner_id][in]=" + rows.ownerB: {"A", "B"},
			"filters[owner_id][nin]=" + rows.ownerB:                                          {"A", "C"},
			"filters[owner_id][empty]=true":                                                  {"C"},
			"filters[photo][empty]=true":                                                     {"B", "C"},
			"filters[photo][empty]=false":                                                    {"A"},
			"filters[meta][empty]=true":                                                      {"B", "C"},
			"filters[meta][empty]=false":                                                     {"A"},
			"filters[id][ne]=" + rows.ids["A"]:                                               {"B", "C"},
			"filters[id][contains]=" + strings.ToLower(rows.ids["A"][:8]):                    {"A"},
			"filters[id][in]=" + rows.ids["A"] + "&filters[id][in]=" + rows.ids["B"]:         {"A", "B"},
			"filters[title][ncontains]=x&filters[rank][gt]=1":                                {"B", "C"},
			"filters[rank][gte]=2&filters[rank][lt]=3":                                       {"B"},
		} {
			assert.ElementsMatch(t, want, rows.names(t, fx, query), query)
		}

		add := func(name string, data map[string]any) string {
			id := fx.createStatus(t, "op_items", "published", data)
			rows.byID[id] = name
			return id
		}
		add("emptyList", map[string]any{"meta": []any{}, "photo": []any{}})
		add("emptyObject", map[string]any{"meta": map[string]any{}, "photo": "b.png"})
		// The write path binds a json string as the document's text, so a
		// JSON null and a JSON empty string are written as text here.
		table := core.TableName("op_items")
		for name, doc := range map[string]string{"jsonNull": "null", "jsonEmptyString": `""`} {
			id := add(name, map[string]any{})
			_, err := fx.pool.Exec(fx.ctx, "UPDATE "+table+" SET meta = '"+doc+"' WHERE id = $1", id)
			require.NoError(t, err, name)
		}
		for query, want := range map[string][]string{
			"filters[meta][empty]=true":   {"B", "C", "emptyList", "emptyObject", "jsonNull", "jsonEmptyString"},
			"filters[meta][empty]=false":  {"A"},
			"filters[photo][empty]=true":  {"B", "C", "emptyList", "jsonNull", "jsonEmptyString"},
			"filters[photo][empty]=false": {"A", "emptyObject"},
		} {
			assert.ElementsMatch(t, want, rows.names(t, fx, query), query)
		}
	})
}

// An operator a field's type does not take, a value its field cannot hold,
// an empty text value outside empty, more than fifty values in one in, more
// than two hundred across a list, and a key the grammar does not read, such
// as an eq after another operator, answer 400, never an empty list or a
// failure on one database. So does every operator but eq on the status and on the
// deletion time, so no operator reaches a draft or a deleted entry, and
// every operator on a to-many relation.
func TestContentList_RefusesAnOperatorItCannotRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"operators"}, func(t *testing.T, fx schemaContentFixture) {
		seedOperatorRows(t, fx)
		tooMany := strings.Repeat("filters[rank][in]=1&", 50) + "filters[rank][in]=2"
		var past []string
		for _, field := range []string{"title", "mail", "link", "notes"} {
			for v := range 50 {
				past = append(past, fmt.Sprintf("filters[%s][in]=v%d", field, v))
			}
		}
		pastTotal := strings.Join(past, "&") + "&filters[rank][ne]=1"
		for _, q := range []string{
			"filters[title][gt]=a", "filters[title][lte]=a", "filters[score][contains]=1", "filters[live][in]=true",
			"filters[live][gt]=true", "filters[day][in]=2026-01-01", "filters[at][contains]=2026",
			"filters[owner_id][contains]=x", "filters[owner_id][gt]=x", "filters[photo][ne]=x",
			"filters[photo][contains]=x", "filters[meta][ne]=x", "filters[meta][in]=x",
			"filters[rank][in]=x", "filters[price][nin]=1.005", "filters[live][ne]=yes", "filters[day][ne]=soon",
			"filters[at][ne]=x", "filters[code][ne]=x", "filters[owner_id][in]=x", "filters[id][nin]=x",
			"filters[title][empty]=maybe", "filters[title][contains]=", "filters[title][bogus]=x",
			"filters[nosuch][ne]=x", "filters[owner][ne]=x", "filters[title][eq]=a&filters[title]=b",
			"filters[deleted_at][empty]=false", "filters[deleted_at][ne]=2026-01-01", "filters[deleted_at][gt]=2026-01-01",
			"filters[_status][ne]=published", "filters[_status][in]=draft", "filters[_status][nin]=published",
			"filters[_status][empty]=false", "filters[_status][contains]=d", "filters[_status][gt]=a",
			"filters[rank][gt][eq]=1", "filters[rank][eq][eq]=1", "filters[title][ne]=", "filters[title][in]=a&filters[title][in]=",
			"filters[mail][nin]=", "filters[notes][ne]=",
			tooMany, pastTotal,
		} {
			rr := fx.listItems(t, q)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
		}
		ok := fx.listItems(t, strings.Repeat("filters[rank][in]=1&", 49)+"filters[rank][in]=2")
		assert.Equal(t, http.StatusOK, ok.Code, "fifty values: %s", ok.Body.String())
	})
	forEachDialectWithSchemas(t, []string{"shelves"}, func(t *testing.T, fx schemaContentFixture) {
		shelf := createdID(t, fx.create(t, "shelves", map[string]any{"name": "s"}))
		for _, field := range []string{"books", "pages"} {
			for _, op := range []string{"eq", "ne", "in", "nin", "empty", "contains"} {
				v := shelf
				if op == "empty" {
					v = "true"
				}
				q := "filters[" + field + "][" + op + "]=" + v
				rr := fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/shelves?"+q,
					map[string]string{"schema": "shelves"}, nil, fx.content.List)
				assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
			}
		}
	})
}

// A contains or ncontains value scans every row it cannot anchor, so one
// past 255 characters is refused with the list's 400, as the delivery list
// refuses one. A value of 255 characters runs.
func TestContentList_ContainsValuePast255CharactersIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"operators"}, func(t *testing.T, fx schemaContentFixture) {
		seedOperatorRows(t, fx)
		for _, op := range []string{"contains", "ncontains"} {
			rr := fx.listItems(t, "filters[title]["+op+"]="+strings.Repeat("a", 256))
			assert.Equal(t, http.StatusBadRequest, rr.Code, "%s of 256 characters: %s", op, rr.Body.String())

			rr = fx.listItems(t, "filters[title]["+op+"]="+url.QueryEscape(strings.Repeat("\u00e9", 255)))
			assert.Equal(t, http.StatusOK, rr.Code, "%s of 255 characters: %s", op, rr.Body.String())
		}
	})
}

// A caller who cannot read drafts reaches no draft through an operator: eq on
// the status answers 403 as the bare filter does, and every other operator
// on it 400. A field the caller's mask hides answers 403 for every operator,
// because each one reads the hidden value a guess at a time. A deleted entry
// stays out for every caller.
func TestContentList_OperatorsKeepDraftsDeletedAndHiddenFieldsOut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"operators"}, func(t *testing.T, fx schemaContentFixture) {
		rows := seedOperatorRows(t, fx)
		assert.ElementsMatch(t, []string{"D"}, rows.names(t, fx, "filters[_status][eq]=draft"), "a writer lists its drafts by eq")
		assert.ElementsMatch(t, []string{"A", "B", "C"}, rows.names(t, fx, "filters[rank][gt]=0"), "a deleted entry or a draft was listed")

		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"op_items"}},
			mask:  map[string][]string{"op_items": {"secret"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})
		get := func(query string) *httptest.ResponseRecorder {
			return call(t, r, http.MethodGet, "reader", "/api/v1/content/op_items?"+query, nil)
		}
		for _, q := range []string{"filters[_status]=draft", "filters[_status][eq]=draft"} {
			assert.Equal(t, http.StatusForbidden, get(q).Code, q)
		}
		for _, op := range []string{"ne", "in", "nin", "empty", "contains", "ncontains", "gt", "gte", "lt", "lte"} {
			v := "published"
			if op == "empty" {
				v = "false"
			}
			rr := get("filters[_status][" + op + "]=" + v)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "_status %s: %s", op, rr.Body.String())
			assert.NotContains(t, rr.Body.String(), rows.ids["D"])
		}
		for _, op := range []string{"eq", "ne", "in", "nin", "empty", "contains", "ncontains", "gt", "gte", "lt", "lte"} {
			v := "s1"
			if op == "empty" {
				v = "false"
			}
			rr := get("filters[secret][" + op + "]=" + v)
			assert.Equal(t, http.StatusForbidden, rr.Code, "secret %s: %s", op, rr.Body.String())
			assert.NotContains(t, rr.Body.String(), "s1")
		}
		rr := get("filters[title][ne]=Alpha")
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.NotContains(t, rr.Body.String(), rows.ids["D"])
		assert.NotContains(t, rr.Body.String(), rows.ids["E"])
		assert.NotContains(t, rr.Body.String(), `"secret"`)
	})
}

// The delivery list takes the operators on the fields it filters, counts an
// in or a nin as one of its four filters whatever its values, takes no more
// than fifty values, keeps its bounds on created_at alone, and answers every
// refusal, a private field's included, as it answers an unknown field. Each
// answer has one URL, so the [eq] spelling of an equality and a value given
// twice in one in or nin are refused.
func TestDelivery_OperatorsWithinTheGrammar(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		one := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "Red 10%", "rank": 1, "featured": true, "secret": "x"})
		two := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "blue", "rank": 2, "featured": false})
		three := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "green", "rank": 3})
		rg.post(t, "t1", "dl_posts", "draft", map[string]any{"title": "red draft", "rank": 1})

		for query, want := range map[string][]string{
			"filters[title][contains]=RED":                       {one},
			"filters[title][contains]=" + url.QueryEscape("%"):   {one},
			"filters[title][ncontains]=e":                        nil,
			"filters[rank][in]=1&filters[rank][in]=3":            {one, three},
			"filters[rank][nin]=1":                               {two, three},
			"filters[featured][ne]=true":                         {two, three},
			"filters[featured][empty]=true":                      {three},
			"filters[id][in]=" + one + "&filters[id][in]=" + two: {one, two},
			"filters[created_at][gt]=2000-01-01T00:00:00Z":       {one, two, three},
			"filters[title]=blue&filters[rank][ne]=1":            {two},
		} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+query, nil)
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s", query, rr.Body.String())
			assert.ElementsMatch(t, want, deliveredIDs(t, rr), query)
		}

		values := make([]string, 50)
		for i := range values {
			values[i] = fmt.Sprintf("filters[rank][in]=%d", i+1)
		}
		fifty := strings.Join(values, "&")
		four := fifty + "&filters[title][ne]=x&filters[featured][empty]=false&filters[created_at][gt]=2000-01-01T00:00:00Z"
		require.Equal(t, http.StatusOK, rg.get(t, deliveryHostA, deliveryPostsPath+"?"+four, nil).Code)

		unknown := rg.get(t, deliveryHostA, deliveryPostsPath+"?filters[nosuch][ne]=x", nil)
		require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
		for _, q := range []string{
			four + "&filters[title][contains]=b",
			"filters[title][ne]=", "filters[title][in]=a&filters[title][in]=",
			fifty + "&filters[rank][in]=51",
			"filters[secret][contains]=x", "filters[secret][empty]=false", "filters[tally][in]=1",
			"filters[rank][gt]=1", "filters[day][lt]=2026-01-01", "filters[updated_at][gt]=2026-01-01",
			"filters[rank][contains]=1", "filters[featured][in]=true", "filters[extra][empty]=true",
			"filters[featured][empty]=yes", "filters[title][contains]=", "filters[rank][ne]=x",
			"filters[author_id][nin]=x", "filters[tags][in]=" + one, "filters[_status][ne]=draft",
			"filters[deleted_at][empty]=false", "filters[title][ne]=a&filters[title][ne]=b",
			"filters[title]=a&filters[title][eq]=b", "filters[title][in]=" + strings.Repeat("a", 256),
		} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
		}
		for _, q := range []string{
			"filters[secret][contains]=x", "filters[secret][empty]=false", "filters[tally][in]=1",
			"filters[title][eq]=blue", "filters[rank][in]=1&filters[rank][in]=1", "filters[rank][nin]=2&filters[rank][in]=1&filters[rank][nin]=2",
			"filters[author.name]=x", "filters[tags.label][ne]=x", "filters[author.badge.label][contains]=x",
		} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)
			assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
		}
	})
}
