package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// fixtureRows are the entries the sort and range tests list: runs of equal
// values longer than a page, and two entries with no value in any sorted
// field.
var fixtureRows = []map[string]any{
	{"title": "a", "score": 2.5, "rank": 3, "price": "1.50", "day": "2026-01-03", "at": "2026-01-03T10:00:00Z"},
	{"title": "b", "score": 1, "rank": 1, "price": "9.00", "day": "2026-01-01", "at": "2026-01-01T10:00:00Z"},
	{"title": "c", "score": 2.5, "rank": 3, "price": "1.50", "day": "2026-01-03", "at": "2026-01-03T10:00:00Z"},
	{"title": "d"},
	{"title": "e", "score": 10, "rank": 2, "price": "3.25", "day": "2026-01-02", "at": "2026-01-02T08:30:00Z"},
	{"title": "f", "score": 2.5, "rank": 3, "price": "1.50", "day": "2026-01-03", "at": "2026-01-03T10:00:00Z"},
	{"title": "g"},
	{"title": "h", "score": 10, "rank": 2, "price": "3.25", "day": "2026-01-02", "at": "2026-01-02T08:30:00Z"},
	{"title": "i", "score": 2.5, "rank": 3, "price": "1.50", "day": "2026-01-03", "at": "2026-01-03T10:00:00Z"},
}

// listedRows decodes a 200 list into each entry's id and data.
func listedRows(t *testing.T, rr *httptest.ResponseRecorder) []fixtureEntry {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out []fixtureEntry
	dec := json.NewDecoder(strings.NewReader(rr.Body.String()))
	dec.UseNumber()
	require.NoError(t, dec.Decode(&out), rr.Body.String())
	return out
}

type fixtureEntry struct {
	ID        string         `json:"id"`
	CreatedAt string         `json:"created_at"`
	Data      map[string]any `json:"data"`
}

// sortKey answers the value an entry sorts by, as a float for the number
// kinds and as text for the rest, and false when it has none.
func sortKey(t *testing.T, e fixtureEntry, field string) (any, bool) {
	t.Helper()
	var v any
	if field == "created_at" {
		v = e.CreatedAt
	} else {
		v = e.Data[field]
	}
	if v == nil {
		return nil, false
	}
	switch field {
	case "score", "rank", "price":
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		require.NoError(t, err)
		return f, true
	case "at", "created_at":
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(v))
		require.NoError(t, err)
		return float64(at.UnixMicro()), true
	}
	return fmt.Sprint(v), true
}

// assertSorted fails unless entries run in field's order, or its reverse
// when desc, with every entry that has no value after every one that has.
func assertSorted(t *testing.T, entries []fixtureEntry, field string, desc bool) {
	t.Helper()
	require.Len(t, entries, len(fixtureRows))
	seenNull := false
	var prev any
	for i, e := range entries {
		v, ok := sortKey(t, e, field)
		if !ok {
			seenNull = true
			continue
		}
		require.False(t, seenNull, "%s: entry %d has a value after an entry with none", field, i)
		if prev != nil {
			var cmp int
			switch p := prev.(type) {
			case float64:
				cmp = compareFloat(p, v.(float64))
			case string:
				cmp = strings.Compare(p, v.(string))
			}
			if desc {
				assert.GreaterOrEqual(t, cmp, 0, "%s desc: %v before %v", field, prev, v)
			} else {
				assert.LessOrEqual(t, cmp, 0, "%s asc: %v before %v", field, prev, v)
			}
		}
		prev = v
	}
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func entryIDs(entries []fixtureEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// seedFixtures writes fixtureRows and returns their ids.
func seedFixtures(t *testing.T, fx schemaContentFixture) []string {
	t.Helper()
	ids := make([]string, 0, len(fixtureRows))
	for _, row := range fixtureRows {
		data := make(map[string]any, len(row))
		for k, v := range row {
			data[k] = v
		}
		ids = append(ids, createdID(t, fx.create(t, "fixtures", data)))
	}
	return ids
}

func (fx schemaContentFixture) listFixtures(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/fixtures?"+query,
		map[string]string{"schema": "fixtures"}, nil, fx.content.List)
}

func (fx schemaContentFixture) cursorFixtures(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return fx.send(t, fx.ctx, http.MethodGet, "/api/v1/content/fixtures/cursor?"+query,
		map[string]string{"schema": "fixtures"}, nil, fx.content.ListCursor)
}

// cursorPages walks the cursor list a page of size at a time, with the sort
// on every request, and returns every entry it was served.
func (fx schemaContentFixture) cursorPages(t *testing.T, sort string, size int) []fixtureEntry {
	t.Helper()
	var all []fixtureEntry
	cursor := ""
	for range len(fixtureRows) + 2 {
		q := url.Values{"limit": {strconv.Itoa(size)}}
		if sort != "" {
			q.Set("sort", sort)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		rr := fx.cursorFixtures(t, q.Encode())
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var page struct {
			Data       []fixtureEntry `json:"data"`
			NextCursor string         `json:"next_cursor"`
		}
		dec := json.NewDecoder(strings.NewReader(rr.Body.String()))
		dec.UseNumber()
		require.NoError(t, dec.Decode(&page), rr.Body.String())
		all = append(all, page.Data...)
		if page.NextCursor == "" {
			return all
		}
		cursor = page.NextCursor
	}
	t.Fatalf("the cursor never ended after %d entries", len(all))
	return nil
}

// A sort orders the list by one field either way, with entries that have no
// value last in both directions on every dialect. Equal values are broken by
// the id, so pages of two over runs of four equal values, by offset and by
// cursor, serve every entry exactly once and in the order of the unpaged
// list.
func TestContentList_SortsByAFieldAcrossPages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"fixtures"}, func(t *testing.T, fx schemaContentFixture) {
		ids := seedFixtures(t, fx)
		for _, field := range []string{"day", "at", "score", "rank", "price", "created_at"} {
			for _, sort := range []string{field, "-" + field} {
				desc := strings.HasPrefix(sort, "-")
				whole := listedRows(t, fx.listFixtures(t, "limit=50&sort="+url.QueryEscape(sort)))
				assertSorted(t, whole, field, desc)
				assert.ElementsMatch(t, ids, entryIDs(whole), sort)

				var paged []fixtureEntry
				for offset := 0; offset < len(fixtureRows)+2; offset += 2 {
					paged = append(paged, listedRows(t, fx.listFixtures(t,
						fmt.Sprintf("limit=2&offset=%d&sort=%s", offset, url.QueryEscape(sort))))...)
				}
				assert.Equal(t, entryIDs(whole), entryIDs(paged), "offset pages of %s", sort)

				assert.Equal(t, entryIDs(whole), entryIDs(fx.cursorPages(t, sort, 2)), "cursor pages of %s", sort)
			}
		}
	})
}

// A list that names no sort keeps the order it had: the offset list newest
// first, the cursor list in id order with the last id as its cursor.
func TestContentList_DefaultOrderIsUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"fixtures"}, func(t *testing.T, fx schemaContentFixture) {
		ids := seedFixtures(t, fx)
		plain := listedRows(t, fx.listFixtures(t, "limit=50"))
		assert.Equal(t, entryIDs(listedRows(t, fx.listFixtures(t, "limit=50&sort=-created_at"))), entryIDs(plain))

		walked := fx.cursorPages(t, "", 2)
		assert.ElementsMatch(t, ids, entryIDs(walked))
		stored, err := fx.content.store.ListCursor(fx.ctx, "fixtures", "", 50, true)
		require.NoError(t, err)
		want := make([]string, len(stored))
		for i, c := range stored {
			want[i] = c.ID.String()
		}
		assert.Equal(t, want, entryIDs(walked), "the cursor list keeps the store's id order")
	})
}

// A range bounds a number, integer, decimal, date or datetime field from
// below, above or both, inclusively, and combines with an equality filter.
func TestContentList_RangesOnNumberDateAndDatetimeFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"fixtures"}, func(t *testing.T, fx schemaContentFixture) {
		seedFixtures(t, fx)
		titles := func(query string) []string {
			t.Helper()
			var out []string
			for _, e := range listedRows(t, fx.listFixtures(t, query)) {
				out = append(out, e.Data["title"].(string))
			}
			return out
		}
		for query, want := range map[string][]string{
			"filters[day][gte]=2026-01-02":                                                {"a", "c", "e", "f", "h", "i"},
			"filters[day][lte]=2026-01-02":                                                {"b", "e", "h"},
			"filters[day][gte]=2026-01-02&filters[day][lte]=2026-01-02":                   {"e", "h"},
			"filters[at][gte]=2026-01-02T08:30:00Z&filters[at][lte]=2026-01-02T23:00:00Z": {"e", "h"},
			"filters[at][gte]=2026-01-03T10:00:00.000001Z":                                nil,
			"filters[score][gte]=2.5":                                                     {"a", "c", "e", "f", "h", "i"},
			"filters[score][lte]=2.5":                                                     {"a", "b", "c", "f", "i"},
			"filters[rank][gte]=2&filters[rank][lte]=2":                                   {"e", "h"},
			"filters[price][lte]=3.25":                                                    {"a", "c", "e", "f", "h", "i"},
			"filters[price][gte]=3.3":                                                     {"b"},
			"filters[day][gte]=2026-01-02&filters[title]=e":                               {"e"},
		} {
			assert.ElementsMatch(t, want, titles("limit=50&"+query), query)
		}
	})
}

// A sort or range the list cannot run answers 400: a field the type does not
// have, a field of a type with no order every dialect agrees on, an operator
// other than gte and lte, a value its field cannot hold, and a malformed
// sort. A sorted cursor that names no entry answers 400 too, rather than an
// empty page that reads as the end of the list.
func TestContentList_RefusesASortOrRangeItCannotRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	forEachDialectWithSchemas(t, []string{"fixtures"}, func(t *testing.T, fx schemaContentFixture) {
		seedFixtures(t, fx)
		for _, q := range []string{
			"sort=nosuch", "sort=-nosuch", "sort=title", "sort=id", "sort=", "sort=-", "sort=day&sort=rank",
			"filters[nosuch][gte]=1", "filters[title][gte]=a", "filters[day][gt]=2026-01-01",
			"filters[day][gte]=soon", "filters[at][lte]=x", "filters[score][gte]=abc", "filters[rank][lte]=1.5",
			"filters[price][gte]=1.005",
		} {
			rr := fx.listFixtures(t, q)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "%s: %s", q, rr.Body.String())
		}
		for _, q := range []string{"sort=nosuch", "sort=title", "sort=day&cursor=" + uuid.NewString()} {
			rr := fx.cursorFixtures(t, q)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "cursor %s: %s", q, rr.Body.String())
		}
	})
}

// A sort or range on a field the caller's mask hides answers 403, as an
// equality filter on it does: the order of the entries, or which of them a
// bound keeps, reads the hidden value without serving it.
func TestContentList_SortOrRangeOnAHiddenFieldIsRefused(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"fixtures"}, func(t *testing.T, fx schemaContentFixture) {
		seedFixtures(t, fx)
		fx.content.perms = fakeProvider{ruleChecker{
			reads: map[string][]string{"reader": {"fixtures"}},
			mask:  map[string][]string{"fixtures": {"rank"}},
		}}
		r := keyRouter(fx, map[string]*core.AuthClaims{
			"reader": {Roles: []string{"reader"}, Scopes: []string{"content:read"}},
		})
		for _, path := range []string{
			"/api/v1/content/fixtures?sort=rank",
			"/api/v1/content/fixtures?sort=-rank",
			"/api/v1/content/fixtures?filters[rank][gte]=2",
			"/api/v1/content/fixtures?filters[rank][lte]=2",
			"/api/v1/content/fixtures/cursor?sort=rank",
			"/api/v1/content/fixtures/cursor?sort=-rank",
		} {
			rr := call(t, r, http.MethodGet, "reader", path, nil)
			assert.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", path, rr.Body.String())
		}
		for _, path := range []string{
			"/api/v1/content/fixtures?sort=-day&filters[score][gte]=2.5",
			"/api/v1/content/fixtures/cursor?sort=-day",
		} {
			rr := call(t, r, http.MethodGet, "reader", path, nil)
			assert.Equal(t, http.StatusOK, rr.Code, "a visible field still sorts: %s: %s", path, rr.Body.String())
			assert.NotContains(t, rr.Body.String(), `"rank"`)
		}
	})
}

// The delivery list sorts and bounds the fields it serves under its own
// grammar: newest first by a date, with entries that have no date last, a
// range counted among its four filters, and a private field refused with the
// answer an unknown one gets.
func TestDelivery_SortsAndBoundsServedFields(t *testing.T) {
	forEachDialectWithSchemas(t, []string{"delivery"}, func(t *testing.T, fx schemaContentFixture) {
		rg := newDeliveryRig(t, fx, nil)
		older := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "older", "day": "2026-03-01", "rank": 1})
		undated := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "undated", "rank": 2})
		newer := rg.post(t, "t1", "dl_posts", "published", map[string]any{"title": "newer", "day": "2026-04-01", "rank": 3})

		assert.Equal(t, []string{newer, older, undated},
			deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?sort=-day", nil)))
		assert.Equal(t, []string{older, newer, undated},
			deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?sort=day", nil)))
		assert.Equal(t, []string{newer, undated},
			deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?sort=-rank&filters[rank][gte]=2", nil)))
		assert.Equal(t, []string{newer},
			deliveredIDs(t, rg.get(t, deliveryHostA, deliveryPostsPath+"?filters[day][gte]=2026-03-15&filters[day][lte]=2026-04-01", nil)))

		unknown := rg.get(t, deliveryHostA, deliveryPostsPath+"?sort=nosuch", nil)
		require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.Body.String())
		for _, q := range []string{"sort=secret", "sort=-secret", "sort=title", "sort=", "sort=-"} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, q)
			assert.JSONEq(t, stripRequestID(t, unknown.Body.String()), stripRequestID(t, rr.Body.String()), q)
		}
		for _, q := range []string{
			"filters[secret][gte]=a", "filters[title][gte]=a", "filters[day][gt]=2026-01-01", "filters[day][gte]=soon",
			"filters[rank][gte]=1&filters[rank][lte]=3&filters[day][gte]=2026-01-01&filters[day][lte]=2026-12-31&filters[title]=x",
		} {
			rr := rg.get(t, deliveryHostA, deliveryPostsPath+"?"+q, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code, q+": "+rr.Body.String())
		}
	})
}
