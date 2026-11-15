package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// The list reads filters[field][op]: eq joins the equality filters, the
// bounds stay for takeRanges, in and nin take every value of their key, the
// other operators take the first, and an unknown operator stays a filter for
// the store to refuse as an unknown column.
func TestTakeConditions_ReadsEachOperator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		query   string
		filters map[string]any
		conds   []db.ListCondition
		ok      bool
	}{
		{"equality unchanged", "filters[title]=a", map[string]any{"title": "a"}, nil, true},
		{"eq is equality", "filters[title][eq]=a", map[string]any{"title": "a"}, nil, true},
		{"eq beside equality", "filters[title][eq]=a&filters[title]=b", nil, nil, false},
		{"bounds stay", "filters[rank][gt]=1&filters[rank][lte]=3", map[string]any{"rank][gt": "1", "rank][lte": "3"}, nil, true},
		{"ne", "filters[title][ne]=a", map[string]any{}, []db.ListCondition{{Column: "title", Op: "ne", Values: []any{"a"}}}, true},
		{"ne first value wins", "filters[title][ne]=a&filters[title][ne]=b", map[string]any{}, []db.ListCondition{{Column: "title", Op: "ne", Values: []any{"a"}}}, true},
		{"contains", "filters[title][contains]=a%25", map[string]any{}, []db.ListCondition{{Column: "title", Op: "contains", Values: []any{"a%"}}}, true},
		{"ncontains", "filters[title][ncontains]=a", map[string]any{}, []db.ListCondition{{Column: "title", Op: "ncontains", Values: []any{"a"}}}, true},
		{"empty", "filters[title][empty]=true", map[string]any{}, []db.ListCondition{{Column: "title", Op: "empty", Values: []any{"true"}}}, true},
		{"in repeated", "filters[rank][in]=1&filters[rank][in]=2", map[string]any{}, []db.ListCondition{{Column: "rank", Op: "in", Values: []any{"1", "2"}}}, true},
		{"nin repeated", "filters[rank][nin]=1&filters[rank][nin]=2", map[string]any{}, []db.ListCondition{{Column: "rank", Op: "nin", Values: []any{"1", "2"}}}, true},
		{"fifty values", strings.Repeat("filters[rank][in]=1&", 50), map[string]any{}, []db.ListCondition{{Column: "rank", Op: "in", Values: repeated("1", 50)}}, true},
		{"fifty-one values", strings.Repeat("filters[rank][in]=1&", 51), nil, nil, false},
		{"two hundred values in all", manyFields(4, 50), map[string]any{}, nil, true},
		{"past two hundred values in all", manyFields(4, 50) + "&filters[x][ne]=1", nil, nil, false},
		{"eq after an operator stays", "filters[rank][gt][eq]=1", map[string]any{"rank][gt][eq": "1"}, nil, true},
		{"unknown operator stays", "filters[title][like]=a", map[string]any{"title][like": "a"}, nil, true},
		{"empty field name stays", "filters[][ne]=a", map[string]any{"][ne": "a"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/content/x?"+tc.query, nil)
			filters, err := parseFilters(r)
			require.NoError(t, err)
			conds, ok := takeConditions(r.URL.Query(), filters)
			require.Equal(t, tc.ok, ok)
			if !ok {
				return
			}
			assert.Equal(t, tc.filters, filters)
			if tc.conds != nil {
				assert.ElementsMatch(t, tc.conds, conds)
			}
		})
	}
}

// gt and lt join gte and lte as bounds, each on its own side.
func TestTakeRanges_ReadsStrictBounds(t *testing.T) {
	filters := map[string]any{"rank][gt": "1", "rank][lt": "5", "day][gte": "2026-01-01", "title": "a"}
	ranges := takeRanges(filters)
	assert.Equal(t, map[string]any{"title": "a"}, filters)
	assert.Equal(t, map[string]db.ListRange{
		"rank": {GT: "1", LT: "5"},
		"day":  {GTE: "2026-01-01"},
	}, ranges)
}

func repeated(v string, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// manyFields writes an in of per values on each of n fields.
func manyFields(n, per int) string {
	parts := make([]string, 0, n*per)
	for f := range n {
		for v := range per {
			parts = append(parts, fmt.Sprintf("filters[f%d][in]=%d", f, v))
		}
	}
	return strings.Join(parts, "&")
}

// An equality or a bound whose key is a path moves into the conditions, with
// eq or the bound's name as its operator, and its values count toward the
// two hundred a list binds.
func TestTakePaths_MovesPathKeysIntoConditions(t *testing.T) {
	filters := map[string]any{"title": "a", "author.name": "Ann"}
	ranges := map[string]db.ListRange{"rank": {GT: "1"}, "comments.rating": {GTE: "2", LT: "5"}}
	conds, ok := takePaths(filters, ranges, []db.ListCondition{{Column: "tags.slug", Op: "ne", Values: []any{"go"}}})
	require.True(t, ok)
	assert.Equal(t, map[string]any{"title": "a"}, filters)
	assert.Equal(t, map[string]db.ListRange{"rank": {GT: "1"}}, ranges)
	assert.ElementsMatch(t, []db.ListCondition{
		{Column: "tags.slug", Op: "ne", Values: []any{"go"}},
		{Column: "author.name", Op: "eq", Values: []any{"Ann"}},
		{Column: "comments.rating", Op: "gte", Values: []any{"2"}},
		{Column: "comments.rating", Op: "lt", Values: []any{"5"}},
	}, conds)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/content/x?"+manyFields(4, 50)+"&filters[author.name]=Ann", nil)
	filters, err := parseFilters(r)
	require.NoError(t, err)
	conds, ok = takeConditions(r.URL.Query(), filters)
	require.True(t, ok)
	_, ok = takePaths(filters, takeRanges(filters), conds)
	assert.False(t, ok, "a path's value did not count toward the two hundred")
}
