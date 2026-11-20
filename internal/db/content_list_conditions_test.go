//go:build !mutest

package db

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

var conditionSchema = &domain.Schema{Fields: []domain.SchemaField{
	{Name: "title", FieldType: "text"},
	{Name: "body", FieldType: "rich_text"},
	{Name: "mail", FieldType: "email"},
	{Name: "link", FieldType: "url"},
	{Name: "code", FieldType: "uid"},
	{Name: "score", FieldType: "number"},
	{Name: "rank", FieldType: "integer"},
	{Name: "price", FieldType: "decimal", Precision: new(10), Scale: new(2)},
	{Name: "live", FieldType: "boolean"},
	{Name: "day", FieldType: "date"},
	{Name: "at", FieldType: "datetime"},
	{Name: "owner", FieldType: "relation", RelationType: domain.RelBelongsTo, RelationTo: "people"},
	{Name: "tags", FieldType: "relation", RelationType: domain.RelManyToMany, RelationTo: "tags"},
	{Name: "photo", FieldType: "media"},
	{Name: "meta", FieldType: "json"},
}}

// Each field type takes the operators of its family and refuses the rest,
// the status and the deletion time take none, and a to-many relation is an
// unknown column.
func TestTypedConditions_OperatorsByFieldType(t *testing.T) {
	all := []string{OpNotEqual, OpContains, OpNotContains, OpIn, OpNotIn, OpEmpty}
	for col, takes := range map[string][]string{
		"title":      all,
		"body":       all,
		"mail":       all,
		"link":       all,
		"code":       all,
		"id":         all,
		"score":      {OpNotEqual, OpIn, OpNotIn, OpEmpty},
		"rank":       {OpNotEqual, OpIn, OpNotIn, OpEmpty},
		"price":      {OpNotEqual, OpIn, OpNotIn, OpEmpty},
		"owner_id":   {OpNotEqual, OpIn, OpNotIn, OpEmpty},
		"live":       {OpNotEqual, OpEmpty},
		"day":        {OpNotEqual, OpEmpty},
		"at":         {OpNotEqual, OpEmpty},
		"created_at": {OpNotEqual, OpEmpty},
		"updated_at": {OpNotEqual, OpEmpty},
		"photo":      {OpEmpty},
		"meta":       {OpEmpty},
		"_status":    nil,
		"deleted_at": nil,
		"tags":       nil,
		"owner":      nil,
	} {
		for _, op := range all {
			_, err := typedConditions(conditionSchema, []ListCondition{{Column: col, Op: op, Values: []any{sampleValue(col, op)}}})
			if slices.Contains(takes, op) {
				assert.NoError(t, err, "%s %s", col, op)
			} else {
				assert.ErrorIs(t, err, domain.ErrBadRequest, "%s %s", col, op)
			}
		}
	}
}

// sampleValue answers a value col can hold under op.
func sampleValue(col, op string) any {
	switch op {
	case OpEmpty:
		return "true"
	case OpContains, OpNotContains:
		return "x"
	}
	switch col {
	case "code", "id", "owner_id":
		return "4f6a3a3e-6f3c-4d0e-9a51-0e1c5b1f3b2a"
	case "score", "rank", "price":
		return "2"
	case "live":
		return "true"
	case "day":
		return "2026-01-02"
	case "at", "created_at", "updated_at":
		return "2026-01-02T10:00:00Z"
	}
	return "x"
}

// Values are parsed by the field's type, so one question is one cache key,
// and a value the field cannot hold, or a count its operator does not take,
// is a bad request.
func TestTypedConditions_ParsesValuesByType(t *testing.T) {
	got, err := typedConditions(conditionSchema, []ListCondition{
		{Column: "rank", Op: OpIn, Values: []any{"2", "3"}},
		{Column: "price", Op: OpNotEqual, Values: []any{"1.50"}},
		{Column: "live", Op: OpNotEqual, Values: []any{"false"}},
		{Column: "at", Op: OpNotEqual, Values: []any{"2026-01-02T12:00:00+02:00"}},
		{Column: "code", Op: OpNotIn, Values: []any{"4F6A3A3E-6F3C-4D0E-9A51-0E1C5B1F3B2A"}},
		{Column: "title", Op: OpEmpty, Values: []any{"false"}},
	})
	require.NoError(t, err)
	assert.Equal(t, []ListCondition{
		{Column: "at", Op: OpNotEqual, Values: []any{time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)}},
		{Column: "code", Op: OpNotIn, Values: []any{"4f6a3a3e-6f3c-4d0e-9a51-0e1c5b1f3b2a"}},
		{Column: "live", Op: OpNotEqual, Values: []any{false}},
		{Column: "price", Op: OpNotEqual, Values: []any{"1.50"}},
		{Column: "rank", Op: OpIn, Values: []any{int64(2), int64(3)}},
		{Column: "title", Op: OpEmpty, Values: []any{false}},
	}, got, "sorted by column, each value typed")

	tooMany := make([]any, MaxListValues+1)
	for i := range tooMany {
		tooMany[i] = "1"
	}
	for name, c := range map[string]ListCondition{
		"integer not a number": {Column: "rank", Op: OpIn, Values: []any{"x"}},
		"decimal past scale":   {Column: "price", Op: OpNotIn, Values: []any{"1.005"}},
		"boolean word":         {Column: "live", Op: OpNotEqual, Values: []any{"yes"}},
		"date":                 {Column: "day", Op: OpNotEqual, Values: []any{"2026-13-01"}},
		"datetime":             {Column: "at", Op: OpNotEqual, Values: []any{"soon"}},
		"uid":                  {Column: "code", Op: OpNotEqual, Values: []any{"x"}},
		"relation":             {Column: "owner_id", Op: OpIn, Values: []any{"x"}},
		"empty not a boolean":  {Column: "title", Op: OpEmpty, Values: []any{"maybe"}},
		"contains nothing":     {Column: "title", Op: OpContains, Values: []any{""}},
		"ne nothing":           {Column: "title", Op: OpNotEqual, Values: []any{""}},
		"in nothing":           {Column: "mail", Op: OpIn, Values: []any{"a@x.io", ""}},
		"nin nothing":          {Column: "body", Op: OpNotIn, Values: []any{""}},
		"in with no value":     {Column: "rank", Op: OpIn},
		"in past the cap":      {Column: "rank", Op: OpIn, Values: tooMany},
		"ne with two values":   {Column: "rank", Op: OpNotEqual, Values: []any{"1", "2"}},
		"unknown operator":     {Column: "rank", Op: "like", Values: []any{"1"}},
	} {
		_, err := typedConditions(conditionSchema, []ListCondition{c})
		assert.True(t, errors.Is(err, domain.ErrBadRequest), "%s: %v", name, err)
	}
}

// The negative operators keep the blank entries, text treats the empty
// string as blank by length on MySQL and SQL Server, contains carries the
// escape clause where the dialect matches by pattern, and a uid is matched
// as text.
func TestConditionClause_SQLPerDialect(t *testing.T) {
	for _, tc := range []struct {
		engine string
		c      ListCondition
		want   string
		args   []any
	}{
		{"postgres", ListCondition{Column: "title", Op: OpNotEqual, Values: []any{"a"}}, `((r."title" IS NULL OR r."title" = '') OR r."title" <> $4)`, []any{"a"}},
		{"mysql", ListCondition{Column: "title", Op: OpEmpty, Values: []any{true}}, "(r.`title` IS NULL OR CHAR_LENGTH(r.`title`) = 0)", nil},
		{"mssql", ListCondition{Column: "title", Op: OpEmpty, Values: []any{false}}, "NOT (r.[title] IS NULL OR DATALENGTH(r.[title]) = 0)", nil},
		{"postgres", ListCondition{Column: "title", Op: OpContains, Values: []any{"5%_"}}, `r."title" ILIKE '%' || $4 || '%' ESCAPE '\'`, []any{`5\%\_`}},
		{"mysql", ListCondition{Column: "title", Op: OpNotContains, Values: []any{"5%"}}, "((r.`title` IS NULL OR CHAR_LENGTH(r.`title`) = 0) OR NOT (r.`title` LIKE CONCAT('%', $4, '%') ESCAPE '\\\\'))", []any{`5\%`}},
		{"mssql", ListCondition{Column: "title", Op: OpContains, Values: []any{"5%"}}, "CHARINDEX($4, r.[title]) > 0", []any{"5%"}},
		{"postgres", ListCondition{Column: "code", Op: OpContains, Values: []any{"4f"}}, `CAST(r."code" AS TEXT) ILIKE '%' || $4 || '%' ESCAPE '\'`, []any{"4f"}},
		{"mssql", ListCondition{Column: "code", Op: OpContains, Values: []any{"4f"}}, "CHARINDEX($4, CAST(r.[code] AS NVARCHAR(36))) > 0", []any{"4f"}},
		{"mysql", ListCondition{Column: "score", Op: OpNotIn, Values: []any{"1", "2"}}, "((r.`score` IS NULL) OR r.`score` NOT IN (CAST($4 AS DECIMAL(38,10)), CAST($5 AS DECIMAL(38,10))))", []any{"1", "2"}},
		{"postgres", ListCondition{Column: "rank", Op: OpIn, Values: []any{int64(1)}}, `r."rank" IN ($4)`, []any{int64(1)}},
		{"postgres", ListCondition{Column: "meta", Op: OpEmpty, Values: []any{true}}, `(r."meta" IS NULL OR r."meta" IN ('null'::jsonb, '""'::jsonb, '[]'::jsonb, '{}'::jsonb))`, nil},
		{"mssql", ListCondition{Column: "meta", Op: OpEmpty, Values: []any{false}}, `NOT (r.[meta] IS NULL OR r.[meta] IN (N'null', N'""', N'[]', N'{}'))`, nil},
		{"postgres", ListCondition{Column: "photo", Op: OpEmpty, Values: []any{true}}, `(r."photo" IS NULL OR r."photo" = '' OR r."photo" = '[]')`, nil},
	} {
		got, args, err := conditionClause(tc.engine, conditionSchema, tc.c, 4)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%s %s %s", tc.engine, tc.c.Column, tc.c.Op)
		assert.Equal(t, tc.args, args, "%s %s %s", tc.engine, tc.c.Column, tc.c.Op)
	}
}

// An operator the clause builder does not know is an error, so a new
// operator added to the grammar but not to the builder fails closed rather
// than running as another operator.
func TestConditionClause_RefusesAnUnknownOperator(t *testing.T) {
	_, _, err := conditionClause("postgres", conditionSchema, ListCondition{Column: "rank", Op: "like", Values: []any{int64(1)}}, 4)
	assert.Error(t, err)
}

// Two lists that differ only in a condition are two cache entries, and a
// list with none keeps the key it had.
func TestCacheListKey_CarriesConditions(t *testing.T) {
	base := ListQuery{Limit: 10, Filters: map[string]any{"title": "a"}}
	assert.Equal(t, `t1:posts:l=10:o=0:f={"title":"a"}`, cacheListKey("t1", "posts", base))

	in := base
	in.Conditions = []ListCondition{{Column: "rank", Op: OpIn, Values: []any{int64(1)}}}
	nin := base
	nin.Conditions = []ListCondition{{Column: "rank", Op: OpNotIn, Values: []any{int64(1)}}}
	keys := map[string]bool{cacheListKey("t1", "posts", base): true, cacheListKey("t1", "posts", in): true, cacheListKey("t1", "posts", nin): true}
	assert.Len(t, keys, 3)

	strict := ListQuery{Limit: 10, Ranges: map[string]ListRange{"rank": {GT: int64(1)}}}
	inclusive := ListQuery{Limit: 10, Ranges: map[string]ListRange{"rank": {GTE: int64(1)}}}
	assert.NotEqual(t, cacheListKey("t1", "posts", strict), cacheListKey("t1", "posts", inclusive))
	assert.Equal(t, `t1:posts:l=10:o=0:r={"rank":{"gte":1}}`, cacheListKey("t1", "posts", inclusive))
}
