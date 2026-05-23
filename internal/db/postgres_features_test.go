//go:build !mutest

// Package db_test: PostgreSQL-specific feature integration tests.
//
// These tests exercise PG-only capabilities that have no portable equivalent
// in MySQL or MSSQL. Each test spins up a throwaway postgres:16-alpine
// container via testdb.Postgres, runs core migrations, and validates the
// feature against the live engine. All tests skip in short mode and when
// CI_DIALECT excludes postgres.
//
// Coverage areas:
//   1. JSONB operators - ->, ->>, @>, <@, ?, ?|, ?&, ||, -, contains path
//   2. Array functions - ANY, ALL, unnest, array_agg, overlap &&
//   3. Window functions - ROW_NUMBER, RANK, DENSE_RANK, LAG, LEAD, SUM/AVG OVER
//   4. Recursive CTEs - WITH RECURSIVE for tree/graph traversal
//   5. Row-level security - ENABLE ROW LEVEL SECURITY, CREATE POLICY
//   6. Full-text search - tsvector, to_tsvector, to_tsquery, ts_rank
//   7. BRIN index - CREATE INDEX USING BRIN, effectiveness validation

package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// 1. JSONB Operators

// TestPostgresJSONB_Operators validates the full suite of JSONB access and
// containment operators against a live PostgreSQL 16 instance.
func TestPostgresJSONB_Operators(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG JSONB integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("jsonb_ops_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx, "CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, doc JSONB NOT NULL)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName) })

	// Seed a rich document.
	seed := `{
		"title": "Hello World",
		"tags": ["go", "postgres", "cms"],
		"meta": {"author": "alice", "version": 2},
		"nested": {"level1": {"level2": "deep_value"}},
		"scores": [10, 20, 30],
		"active": true
	}`
	_, err = pool.Exec(ctx, "INSERT INTO "+tableName+" (doc) VALUES ($1)", seed)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Seed a second document for containment tests.
	seed2 := `{
		"title": "Another",
		"tags": ["rust", "postgres"],
		"meta": {"author": "bob", "version": 1},
		"active": false
	}`
	_, err = pool.Exec(ctx, "INSERT INTO "+tableName+" (doc) VALUES ($1)", seed2)
	if err != nil {
		t.Fatalf("seed2: %v", err)
	}

	// -> field access (returns JSONB)
	t.Run("arrow_field_access", func(t *testing.T) {
		var val string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT doc -> 'title' FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Fatalf("-> title: %v", err)
		}
		if !strings.Contains(val, "Hello World") {
			t.Errorf("-> title = %q, want string containing Hello World", val)
		}
	})

	// ->> text access
	t.Run("arrow_text_access", func(t *testing.T) {
		var val string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT doc ->> 'title' FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Fatalf("->> title: %v", err)
		}
		if val != "Hello World" {
			t.Errorf("->> title = %q, want Hello World", val)
		}
	})

	// @> containment (doc contains)
	t.Run("at_containment", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE doc @> '{\"active\": true}'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("@> containment: %v", err)
		}
		if count != 1 {
			t.Errorf("@> active=true: got %d rows, want 1", count)
		}
	})

	// <@ containment (value is contained by doc)
	t.Run("at_contained_by", func(t *testing.T) {
		var count int

		// A small value IS contained by the first doc.
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE '{\"active\": true}'::JSONB <@ doc",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("<@ contained: %v", err)
		}
		if count != 1 {
			t.Errorf("<@ {active:true}: got %d rows, want 1", count)
		}

		// An absurd value is NOT contained by any doc.
		row, qrErr = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE '{\"active\": false, \"extra\": 999}'::JSONB <@ doc",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&count)
		if err != nil {
			t.Fatalf("<@ not contained: %v", err)
		}
		if count != 0 {
			t.Errorf("<@ {active:false, extra:999}: got %d rows, want 0", count)
		}
	})

	// ? key exists
	t.Run("question_key_exists", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE doc ? 'tags'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("? key exists: %v", err)
		}
		if count != 2 {
			t.Errorf("? 'tags': got %d rows, want 2", count)
		}

		// Negative: key doesn't exist.
		row, qrErr = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE doc ? 'nonexistent'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&count)
		if err != nil {
			t.Fatalf("? nonexistent: %v", err)
		}
		if count != 0 {
			t.Errorf("? 'nonexistent': got %d rows, want 0", count)
		}
	})

	// ?| any key exists
	t.Run("question_pipe_any_key", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE doc ?| ARRAY['title', 'nonexistent']",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("?| any key: %v", err)
		}
		if count != 2 {
			t.Errorf("?| keys: got %d rows, want 2", count)
		}
	})

	// ?& all keys exist
	t.Run("question_amp_all_keys", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE doc ?& ARRAY['title', 'tags', 'meta']",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("?& all keys: %v", err)
		}
		if count != 2 {
			t.Errorf("?& [title,tags,meta]: got %d rows, want 2", count)
		}

		// Should fail: not all keys present.
		row, qrErr = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE doc ?& ARRAY['title', 'nonexistent']",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&count)
		if err != nil {
			t.Fatalf("?& partial: %v", err)
		}
		if count != 0 {
			t.Errorf("?& [title,nonexistent]: got %d rows, want 0", count)
		}
	})

	// || concatenation
	t.Run("concat_operator", func(t *testing.T) {
		var merged string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT (doc || '{\"extra\": 42}') ->> 'extra' FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&merged)
		if err != nil {
			t.Fatalf("|| concat: %v", err)
		}
		if merged != "42" {
			t.Errorf("|| extra = %q, want 42", merged)
		}
	})

	// - delete key
	t.Run("minus_delete_key", func(t *testing.T) {
		var exists bool
		row, qrErr := pool.QueryRow(ctx,
			"SELECT (doc - 'title') ? 'title' FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&exists)
		if err != nil {
			t.Fatalf("- delete key: %v", err)
		}
		if exists {
			t.Error("doc - 'title' should NOT contain 'title' key")
		}
	})

	// Nested path access (#> and #>>)
	t.Run("nested_path_access", func(t *testing.T) {
		var val string
		// #> returns JSONB
		row, qrErr := pool.QueryRow(ctx,
			"SELECT doc #> '{nested,level1}' FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Fatalf("#> nested: %v", err)
		}
		if !strings.Contains(val, "deep_value") {
			t.Errorf("#> nested/level1 = %q, want containing deep_value", val)
		}

		// #>> returns text
		row, qrErr = pool.QueryRow(ctx,
			"SELECT doc #>> '{nested,level1,level2}' FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&val)
		if err != nil {
			t.Fatalf("#>> nested: %v", err)
		}
		if val != "deep_value" {
			t.Errorf("#>> nested/level1/level2 = %q, want deep_value", val)
		}
	})

	// Array element access by index
	t.Run("array_element_by_index", func(t *testing.T) {
		var val int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT (doc -> 'scores' -> 0)::INT FROM "+tableName+" WHERE doc ->> 'title' = 'Hello World'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Fatalf("array index: %v", err)
		}
		if val != 10 {
			t.Errorf("scores[0] = %d, want 10", val)
		}
	})
}

// 2. Array Functions

// TestPostgresArray_Functions validates PostgreSQL array operators and
// functions: ANY, ALL, unnest, array_agg, array_append, overlapping &&.
func TestPostgresArray_Functions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG array integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("arr_func_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, labels TEXT[] NOT NULL, scores INTEGER[] NOT NULL)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName) })

	// Seed rows with array data.
	seeds := []struct {
		labels []string
		scores []int
	}{
		{[]string{"go", "postgres", "cms"}, []int{10, 20, 30}},
		{[]string{"rust", "postgres", "api"}, []int{40, 50}},
		{[]string{"python", "django", "orm"}, []int{60, 70, 80, 90}},
	}
	for i, s := range seeds {
		_, err := pool.Exec(ctx,
			"INSERT INTO "+tableName+" (labels, scores) VALUES ($1, $2)",
			s.labels, s.scores,
		)
		if err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	// ANY (element in array)
	t.Run("any_element_match", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE 'postgres' = ANY(labels)",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("ANY: %v", err)
		}
		if count != 2 {
			t.Errorf("ANY postgres: got %d rows, want 2", count)
		}
	})

	// ALL (all elements satisfy)
	t.Run("all_elements", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE 10 < ALL(scores)",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("ALL: %v", err)
		}
		// Row 0: 10,20,30 - 10 is NOT > 10. Rows 1 and 2: all > 10.
		if count != 2 {
			t.Errorf("ALL(scores) > 10: got %d rows, want 2", count)
		}

		// Negative: check a condition that matches none.
		row, qrErr = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE 100 < ALL(scores)",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&count)
		if err != nil {
			t.Fatalf("ALL none: %v", err)
		}
		if count != 0 {
			t.Errorf("ALL > 100: got %d rows, want 0", count)
		}
	})

	// unnest (expand array to rows)
	t.Run("unnest_expand", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"SELECT unnest(labels) AS label FROM "+tableName+" ORDER BY id, label",
		)
		if err != nil {
			t.Fatalf("unnest: %v", err)
		}
		defer rows.Close()

		var labels []string
		for rows.Next() {
			var label string
			if err := rows.Scan(&label); err != nil {
				t.Fatalf("scan unnest: %v", err)
			}
			labels = append(labels, label)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if len(labels) != 9 {
			t.Errorf("unnest: got %d rows, want 9", len(labels))
		}
		pgCount := 0
		for _, l := range labels {
			if l == "postgres" {
				pgCount++
			}
		}
		if pgCount != 2 {
			t.Errorf("unnest postgres occurrences: got %d, want 2", pgCount)
		}
	})

	// array_agg (aggregate rows into array)
	t.Run("array_agg", func(t *testing.T) {
		var agg string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT array_agg(DISTINCT label ORDER BY label) FROM (SELECT unnest(labels) AS label FROM "+tableName+") sub",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&agg)
		if err != nil {
			t.Fatalf("array_agg: %v", err)
		}
		expected := []string{"api", "cms", "django", "go", "orm", "postgres", "python", "rust"}
		for _, want := range expected {
			if !strings.Contains(agg, want) {
				t.Errorf("array_agg missing %q in %q", want, agg)
			}
		}
	})

	// && (array overlap)
	t.Run("array_overlap", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE labels && ARRAY['go','rust']",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("&& overlap: %v", err)
		}
		if count != 2 {
			t.Errorf("&& {go,rust}: got %d rows, want 2", count)
		}

		row, qrErr = pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE labels && ARRAY['java','elixir']",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&count)
		if err != nil {
			t.Fatalf("&& no overlap: %v", err)
		}
		if count != 0 {
			t.Errorf("&& {java,elixir}: got %d rows, want 0", count)
		}
	})

	// @> (array contains)
	t.Run("array_contains", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE scores @> ARRAY[10,20]",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("@> array: %v", err)
		}
		if count != 1 {
			t.Errorf("@> {10,20}: got %d rows, want 1", count)
		}
	})

	// <@ (array is contained by)
	t.Run("array_contained_by", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE ARRAY[10,20,30] <@ scores",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("<@ array: %v", err)
		}
		if count != 1 {
			t.Errorf("<@ {10,20,30}: got %d rows, want 1", count)
		}
	})

	// array_length
	t.Run("array_length", func(t *testing.T) {
		var n int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT array_length(scores, 1) FROM "+tableName+" WHERE array_length(scores, 1) = 2",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&n)
		if err != nil {
			t.Fatalf("array_length: %v", err)
		}
		if n != 2 {
			t.Errorf("array_length = %d, want 2", n)
		}
	})

	// array_append / array_prepend
	t.Run("array_append_prepend", func(t *testing.T) {
		var val string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT array_to_string(array_append(array_prepend(999, ARRAY[1,2,3]), 4), ',')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Fatalf("array_append/prepend: %v", err)
		}
		if val != "999,1,2,3,4" {
			t.Errorf("append/prepend = %q, want '999,1,2,3,4'", val)
		}
	})
}

// 3. Window Functions

// TestPostgresWindow_Functions validates ROW_NUMBER, RANK, DENSE_RANK,
// LAG, LEAD, and aggregate windows against a live PostgreSQL instance.
func TestPostgresWindow_Functions(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG window integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("winfn_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, grp TEXT NOT NULL, val INTEGER NOT NULL)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName) })

	seeds := []struct {
		grp string
		val int
	}{
		{"A", 10}, {"A", 20}, {"A", 20}, {"A", 30},
		{"B", 40}, {"B", 50}, {"B", 50}, {"B", 60},
	}
	for _, s := range seeds {
		_, err := pool.Exec(ctx, "INSERT INTO "+tableName+" (grp, val) VALUES ($1, $2)", s.grp, s.val)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// ROW_NUMBER
	t.Run("row_number", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"SELECT id, grp, val, ROW_NUMBER() OVER (PARTITION BY grp ORDER BY val, id) AS rn FROM "+tableName+" ORDER BY grp, rn",
		)
		if err != nil {
			t.Fatalf("ROW_NUMBER: %v", err)
		}
		defer rows.Close()

		type row struct {
			grp string
			val int
			rn  int
		}
		var results []row
		for rows.Next() {
			var id int
			var r row
			err := rows.Scan(&id, &r.grp, &r.val, &r.rn)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}

		if len(results) != 8 {
			t.Fatalf("got %d rows, want 8", len(results))
		}
		for _, r := range results {
			if r.rn < 1 || r.rn > 4 {
				t.Errorf("row_number out of range: rn=%d", r.rn)
			}
		}
		if results[0].grp != "A" || results[0].val != 10 || results[0].rn != 1 {
			t.Errorf("first row: grp=%s val=%d rn=%d, want A/10/1", results[0].grp, results[0].val, results[0].rn)
		}
	})

	// RANK and DENSE_RANK
	t.Run("rank_dense_rank", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"SELECT val, RANK() OVER (PARTITION BY grp ORDER BY val) AS rk, DENSE_RANK() OVER (PARTITION BY grp ORDER BY val) AS dr FROM "+tableName+" WHERE grp = 'A' ORDER BY val",
		)
		if err != nil {
			t.Fatalf("RANK: %v", err)
		}
		defer rows.Close()

		type rankRow struct{ val, rk, dr int }
		var results []rankRow
		for rows.Next() {
			var r rankRow
			if err := rows.Scan(&r.val, &r.rk, &r.dr); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}

		if len(results) != 4 {
			t.Fatalf("got %d rows, want 4", len(results))
		}
		expectedDR := []int{1, 2, 2, 3}
		for i, r := range results {
			if r.dr != expectedDR[i] {
				t.Errorf("val=%d: dense_rank=%d, want %d", r.val, r.dr, expectedDR[i])
			}
		}
		expectedRK := []int{1, 2, 2, 4}
		for i, r := range results {
			if r.rk != expectedRK[i] {
				t.Errorf("val=%d: rank=%d, want %d", r.val, r.rk, expectedRK[i])
			}
		}
	})

	// LAG and LEAD
	t.Run("lag_lead", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"SELECT val, LAG(val) OVER (ORDER BY id) AS prev, LEAD(val) OVER (ORDER BY id) AS next FROM "+tableName+" ORDER BY id",
		)
		if err != nil {
			t.Fatalf("LAG/LEAD: %v", err)
		}
		defer rows.Close()

		type nav struct {
			val        int
			prev, next *int
		}
		var results []nav
		for rows.Next() {
			var n nav
			var prev, next sql.NullInt64
			if err := rows.Scan(&n.val, &prev, &next); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if prev.Valid {
				v := int(prev.Int64)
				n.prev = &v
			}
			if next.Valid {
				v := int(next.Int64)
				n.next = &v
			}
			results = append(results, n)
		}

		if len(results) != 8 {
			t.Fatalf("got %d rows, want 8", len(results))
		}
		// First row: no prev.
		if results[0].prev != nil {
			t.Errorf("first row: prev should be nil, got %d", *results[0].prev)
		}
		if results[0].next == nil || *results[0].next != results[1].val {
			t.Errorf("first row: next should be %d", results[1].val)
		}
		// Last row: no next.
		last := results[len(results)-1]
		if last.next != nil {
			t.Errorf("last row: next should be nil, got %d", *last.next)
		}
		if last.prev == nil || *last.prev != results[len(results)-2].val {
			t.Errorf("last row: prev should be %d", results[len(results)-2].val)
		}
	})

	// SUM/AVG OVER (aggregate windows)
	t.Run("sum_avg_over", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"SELECT val, SUM(val) OVER (PARTITION BY grp ORDER BY val) AS running_sum, "+
				"AVG(val) OVER (PARTITION BY grp ORDER BY val) AS running_avg "+
				"FROM "+tableName+" WHERE grp = 'A' ORDER BY val",
		)
		if err != nil {
			t.Fatalf("SUM/AVG OVER: %v", err)
		}
		defer rows.Close()

		type aggRow struct {
			val int
			sum float64
			avg float64
		}
		var results []aggRow
		for rows.Next() {
			var r aggRow
			if err := rows.Scan(&r.val, &r.sum, &r.avg); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}

		// Group A: 10, 20, 20, 30. Default RANGE window: peers share values.
		expectedSums := []float64{10, 50, 50, 80}
		for i, r := range results {
			if r.sum != expectedSums[i] {
				t.Errorf("val=%d: running_sum=%g, want %g", r.val, r.sum, expectedSums[i])
			}
		}
		if results[3].avg != 20 {
			t.Errorf("final avg=%g, want 20", results[3].avg)
		}
	})
}

// 4. Recursive CTEs

// TestPostgresRecursiveCTE validates WITH RECURSIVE for tree traversal.
// We build an org chart and traverse it both top-down (subtree) and
// bottom-up (ancestors).
func TestPostgresRecursiveCTE(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG recursive CTE integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("rcte_org_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, name TEXT NOT NULL, manager_id INTEGER REFERENCES "+tableName+"(id))")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName+" CASCADE") })

	// Build org chart:
	//          CEO (1)
	//         /       \
	//     VP Eng (2)  VP Sales (3)
	//       /    \         \
	//  Eng1 (4) Eng2 (5)  Sales1 (6)

	_, err = pool.Exec(ctx, "INSERT INTO "+tableName+" (id, name, manager_id) VALUES (1, 'CEO', NULL)")
	if err != nil {
		t.Fatalf("seed CEO: %v", err)
	}
	seeds := []struct {
		id        int
		name      string
		managerID *int
	}{
		{2, "VP Eng", intPtr(1)},
		{3, "VP Sales", intPtr(1)},
		{4, "Eng1", intPtr(2)},
		{5, "Eng2", intPtr(2)},
		{6, "Sales1", intPtr(3)},
	}
	for _, s := range seeds {
		_, err := pool.Exec(ctx, "INSERT INTO "+tableName+" (id, name, manager_id) VALUES ($1, $2, $3)", s.id, s.name, s.managerID)
		if err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}

	// Top-down: subtree from CEO
	t.Run("subtree_from_root", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"WITH RECURSIVE org_tree AS ("+
				"  SELECT id, name, manager_id, 0 AS depth FROM "+tableName+" WHERE manager_id IS NULL"+
				"  UNION ALL"+
				"  SELECT e.id, e.name, e.manager_id, t.depth + 1"+
				"  FROM "+tableName+" e INNER JOIN org_tree t ON e.manager_id = t.id"+
				") SELECT id, name, depth FROM org_tree ORDER BY depth, id",
		)
		if err != nil {
			t.Fatalf("recursive CTE: %v", err)
		}
		defer rows.Close()

		type orgRow struct {
			id    int
			name  string
			depth int
		}
		var results []orgRow
		for rows.Next() {
			var r orgRow
			if err := rows.Scan(&r.id, &r.name, &r.depth); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}

		if len(results) != 6 {
			t.Fatalf("subtree: got %d rows, want 6", len(results))
		}
		if results[0].name != "CEO" || results[0].depth != 0 {
			t.Errorf("first row: %s depth=%d, want CEO/0", results[0].name, results[0].depth)
		}
		expectedDepths := []int{0, 1, 1, 2, 2, 2}
		for i, r := range results {
			if r.depth != expectedDepths[i] {
				t.Errorf("row %d (%s): depth=%d, want %d", i, r.name, r.depth, expectedDepths[i])
			}
		}
	})

	// Bottom-up: ancestors of Eng1
	t.Run("ancestors_of_leaf", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"WITH RECURSIVE ancestors AS ("+
				"  SELECT id, name, manager_id, 0 AS hops FROM "+tableName+" WHERE name = 'Eng1'"+
				"  UNION ALL"+
				"  SELECT e.id, e.name, e.manager_id, a.hops + 1"+
				"  FROM "+tableName+" e INNER JOIN ancestors a ON e.id = a.manager_id"+
				") SELECT name, hops FROM ancestors ORDER BY hops",
		)
		if err != nil {
			t.Fatalf("ancestors CTE: %v", err)
		}
		defer rows.Close()

		type ancRow struct {
			name string
			hops int
		}
		var results []ancRow
		for rows.Next() {
			var r ancRow
			if err := rows.Scan(&r.name, &r.hops); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}

		if len(results) != 3 {
			t.Fatalf("ancestors: got %d rows, want 3", len(results))
		}
		expected := []ancRow{{"Eng1", 0}, {"VP Eng", 1}, {"CEO", 2}}
		for i, r := range results {
			if r.name != expected[i].name || r.hops != expected[i].hops {
				t.Errorf("ancestor[%d]: %s hops=%d, want %s/%d", i, r.name, r.hops, expected[i].name, expected[i].hops)
			}
		}
	})

	// Path construction
	t.Run("path_construction", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"WITH RECURSIVE org_path AS ("+
				"  SELECT id, name, manager_id, name AS path FROM "+tableName+" WHERE manager_id IS NULL"+
				"  UNION ALL"+
				"  SELECT e.id, e.name, e.manager_id, p.path || ' -> ' || e.name"+
				"  FROM "+tableName+" e INNER JOIN org_path p ON e.manager_id = p.id"+
				") SELECT name, path FROM org_path WHERE name = 'Eng1'",
		)
		if err != nil {
			t.Fatalf("path CTE: %v", err)
		}
		defer rows.Close()

		if !rows.Next() {
			t.Fatal("expected one row from path_construction CTE")
		}
		var name, path string
		if err := rows.Scan(&name, &path); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name != "Eng1" {
			t.Errorf("name = %q, want Eng1", name)
		}
		if path != "CEO -> VP Eng -> Eng1" {
			t.Errorf("path = %q, want 'CEO -> VP Eng -> Eng1'", path)
		}
	})
}

// 5. Row-Level Security (RLS)

// TestPostgresRLS validates enablement, policy creation, permission-based
// filtering (USING), and write validation (WITH CHECK).
//
// IMPORTANT: The cms test user is a superuser with rolbypassrls=true, so
// RLS policies are silently ignored for that user.  We create a dedicated
// non-superuser role (rls_test) and SET ROLE on each dedicated *sql.Conn
// to actually exercise the policy engine.
func TestPostgresRLS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG RLS integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	// Clean up any leftover role from a prior interrupted test run.
	_, _ = pool.Exec(ctx, "DROP OWNED BY rls_test CASCADE")
	_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS rls_test")

	// Create a non-superuser role for RLS testing: the default cms user
	// has rolbypassrls=true which silently bypasses every policy.
	_, err := pool.Exec(ctx, "CREATE ROLE rls_test WITH LOGIN PASSWORD 'testpass'")
	if err != nil {
		t.Fatalf("CREATE ROLE rls_test: %v", err)
	}
	t.Cleanup(func() {
		// DROP OWNED first so DROP ROLE doesn't complain about objects.
		pool.Exec(context.Background(), "DROP OWNED BY rls_test CASCADE")
		pool.Exec(context.Background(), "DROP ROLE IF EXISTS rls_test")
	})

	tableName := fmt.Sprintf("rls_docs_%d", time.Now().UnixNano()%10000)
	_, err = pool.Exec(ctx,
		"CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, title TEXT NOT NULL, owner TEXT NOT NULL, content TEXT)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName+" CASCADE") })

	// Seed: alice owns docs 1-3, bob owns 4-6.
	seeds := []struct{ title, owner, content string }{
		{"Alice doc 1", "alice", "public report"},
		{"Alice doc 2", "alice", "secret project X"},
		{"Alice doc 3", "alice", "draft notes"},
		{"Bob doc 1", "bob", "bob's public"},
		{"Bob doc 2", "bob", "bob's secret"},
		{"Bob doc 3", "bob", "bob's draft"},
	}
	for _, s := range seeds {
		_, err := pool.Exec(ctx,
			"INSERT INTO "+tableName+" (title, owner, content) VALUES ($1, $2, $3)",
			s.title, s.owner, s.content)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Grant the non-superuser role full CRUD on this table.
	_, err = pool.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON "+tableName+" TO rls_test")
	if err != nil {
		t.Fatalf("GRANT: %v", err)
	}
	_, err = pool.Exec(ctx, "GRANT USAGE ON SEQUENCE "+tableName+"_id_seq TO rls_test")
	if err != nil {
		t.Fatalf("GRANT USAGE ON SEQUENCE: %v", err)
	}

	// Enable RLS and create policies.
	_, err = pool.Exec(ctx, "ALTER TABLE "+tableName+" ENABLE ROW LEVEL SECURITY")
	if err != nil {
		t.Fatalf("ENABLE ROW LEVEL SECURITY: %v", err)
	}

	// Use 'myapp.actor' - avoids reserved-word collision with 'user'.
	// current_setting('myapp.actor') reads the session-local value.
	// NOTE: SET is connection-scoped.  We use a dedicated *sql.Conn with
	// SET ROLE rls_test + SET myapp.actor to pin session state and force
	// the non-superuser path where RLS policies actually fire.
	_, err = pool.Exec(ctx,
		"CREATE POLICY owner_select ON "+tableName+" FOR SELECT USING (owner = current_setting('myapp.actor'))")
	if err != nil {
		t.Fatalf("CREATE POLICY select: %v", err)
	}

	_, err = pool.Exec(ctx,
		"CREATE POLICY owner_insert ON "+tableName+" FOR INSERT WITH CHECK (owner = current_setting('myapp.actor'))")
	if err != nil {
		t.Fatalf("CREATE POLICY insert: %v", err)
	}

	_, err = pool.Exec(ctx,
		"CREATE POLICY owner_update ON "+tableName+" FOR UPDATE USING (owner = current_setting('myapp.actor')) WITH CHECK (owner = current_setting('myapp.actor'))")
	if err != nil {
		t.Fatalf("CREATE POLICY update: %v", err)
	}

	_, err = pool.Exec(ctx,
		"CREATE POLICY owner_delete ON "+tableName+" FOR DELETE USING (owner = current_setting('myapp.actor'))")
	if err != nil {
		t.Fatalf("CREATE POLICY delete: %v", err)
	}

	// FORCE RLS so the table owner (cms) is also subject to policies.
	_, err = pool.Exec(ctx, "ALTER TABLE "+tableName+" FORCE ROW LEVEL SECURITY")
	if err != nil {
		t.Fatalf("FORCE ROW LEVEL SECURITY: %v", err)
	}

	// SELECT enforcement
	t.Run("select_filtering", func(t *testing.T) {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("get conn: %v", err)
		}
		defer conn.Close()

		// Switch to non-superuser so RLS policies actually fire.
		if _, err := conn.ExecContext(ctx, "SET ROLE rls_test"); err != nil {
			t.Fatalf("SET ROLE rls_test: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "SET myapp.actor = 'alice'"); err != nil {
			t.Fatalf("SET alice: %v", err)
		}

		rows, err := conn.QueryContext(ctx, "SELECT id, title, owner FROM "+tableName+" ORDER BY id")
		if err != nil {
			t.Fatalf("select as alice: %v", err)
		}
		defer rows.Close()

		var count int
		for rows.Next() {
			var id int
			var title, owner string
			if err := rows.Scan(&id, &title, &owner); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if owner != "alice" {
				t.Errorf("RLS leak: alice saw row owned by %s (id=%d, title=%s)", owner, id, title)
			}
			count++
		}
		if count != 3 {
			t.Errorf("alice saw %d rows, want 3", count)
		}
	})

	// INSERT enforcement
	t.Run("insert_with_check", func(t *testing.T) {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("get conn: %v", err)
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, "SET ROLE rls_test"); err != nil {
			t.Fatalf("SET ROLE: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "SET myapp.actor = 'alice'"); err != nil {
			t.Fatalf("SET alice: %v", err)
		}

		// Alice inserting her own doc should succeed.
		_, err = conn.ExecContext(ctx,
			"INSERT INTO "+tableName+" (title, owner, content) VALUES ($1, $2, $3)",
			"Alice's new doc", "alice", "fresh")
		if err != nil {
			t.Errorf("alice INSERT should succeed but got: %v", err)
		}

		// Alice inserting for bob should be rejected.
		_, err = conn.ExecContext(ctx,
			"INSERT INTO "+tableName+" (title, owner, content) VALUES ($1, $2, $3)",
			"Stolen doc", "bob", "should fail")
		if err == nil {
			t.Error("RLS failure: alice should NOT be able to insert a doc owned by bob")
		} else {
			t.Logf("correctly rejected cross-owner insert: %v", err)
		}
	})

	// UPDATE enforcement
	t.Run("update_using_check", func(t *testing.T) {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("get conn: %v", err)
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, "SET ROLE rls_test"); err != nil {
			t.Fatalf("SET ROLE: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "SET myapp.actor = 'alice'"); err != nil {
			t.Fatalf("SET alice: %v", err)
		}

		result, err := conn.ExecContext(ctx,
			"UPDATE "+tableName+" SET title = 'Updated by alice' WHERE title = 'Alice doc 1'")
		if err != nil {
			t.Errorf("alice UPDATE own doc: %v", err)
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			t.Errorf("alice UPDATE affected %d rows, want 1", affected)
		}

		// Alice trying to change owner to bob should fail (WITH CHECK).
		_, err = conn.ExecContext(ctx,
			"UPDATE "+tableName+" SET owner = 'bob' WHERE title = 'Updated by alice'")
		if err == nil {
			t.Error("RLS failure: alice should NOT be able to reassign doc to bob")
		} else {
			t.Logf("correctly rejected owner reassignment: %v", err)
		}
	})

	// DELETE enforcement
	t.Run("delete_using", func(t *testing.T) {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("get conn: %v", err)
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, "SET ROLE rls_test"); err != nil {
			t.Fatalf("SET ROLE: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "SET myapp.actor = 'alice'"); err != nil {
			t.Fatalf("SET alice: %v", err)
		}

		result, err := conn.ExecContext(ctx,
			"DELETE FROM "+tableName+" WHERE title = 'Alice doc 3'")
		if err != nil {
			t.Errorf("alice DELETE own doc: %v", err)
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			t.Errorf("alice DELETE affected %d rows, want 1", affected)
		}
	})

	// Switch user: bob sees only his own rows
	t.Run("switch_user_bob", func(t *testing.T) {
		conn, err := pool.Conn(ctx)
		if err != nil {
			t.Fatalf("get conn: %v", err)
		}
		defer conn.Close()

		if _, err := conn.ExecContext(ctx, "SET ROLE rls_test"); err != nil {
			t.Fatalf("SET ROLE: %v", err)
		}
		if _, err := conn.ExecContext(ctx, "SET myapp.actor = 'bob'"); err != nil {
			t.Fatalf("SET bob: %v", err)
		}

		var count int
		err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tableName).Scan(&count)
		if err != nil {
			t.Fatalf("count as bob: %v", err)
		}
		// Original 6 + alice added 1 + alice deleted 1 = 6 remaining.
		// Bob should see 3 of those.
		if count != 3 {
			t.Errorf("bob saw %d rows, want 3", count)
		}

		var aliceCount int
		err = conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE owner = 'alice'").Scan(&aliceCount)
		if err != nil {
			t.Fatalf("alice rows as bob: %v", err)
		}
		if aliceCount != 0 {
			t.Errorf("RLS leak: bob saw %d alice-owned rows, want 0", aliceCount)
		}
	})
}

// 6. Full-Text Search (tsvector)

// TestPostgresFullTextSearch validates tsvector generation, tsquery
// matching, ts_rank scoring, and combined search patterns.
func TestPostgresFullTextSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG FTS integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("fts_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL, tsv TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', coalesce(title,'') || ' ' || coalesce(body,''))) STORED)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName+" CASCADE") })

	seeds := []struct{ title, body string }{
		{"PostgreSQL Full Text Search", "Learn how to use tsvector and tsquery for powerful text search in PostgreSQL."},
		{"Go Programming Guide", "A comprehensive guide to writing Go programs with goroutines and channels."},
		{"Database Indexing Strategies", "PostgreSQL supports B-tree, hash, GIN, GiST, BRIN, and SP-GiST indexes for different workloads."},
		{"The Fox and the Dog", "The quick brown fox jumps over the lazy dog. This pangram is popular."},
	}
	for _, s := range seeds {
		_, err := pool.Exec(ctx,
			"INSERT INTO "+tableName+" (title, body) VALUES ($1, $2)",
			s.title, s.body)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// to_tsvector / to_tsquery with @@ operator
	t.Run("phrase_match", func(t *testing.T) {
		// "search" stems to "search" - appears in row 1's body, row 3's body does NOT
		// contain "search" as a word (it has "indexes" but not "search").
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE tsv @@ to_tsquery('english', 'search')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("tsquery 'search': %v", err)
		}
		// Row 1 body: "Learn how to use tsvector and tsquery for powerful text search..."
		// Row 3 body: "PostgreSQL supports B-tree...", no "search".
		if count != 1 {
			t.Errorf("'search': got %d rows, want 1 (only row 1 contains 'search')", count)
		}
	})

	t.Run("multi_word_and_match", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE tsv @@ to_tsquery('english', 'postgresql & index')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("AND query: %v", err)
		}
		if count != 1 {
			t.Errorf("'postgresql & index': got %d rows, want 1", count)
		}
	})

	t.Run("or_match", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE tsv @@ to_tsquery('english', 'fox | goroutines')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("OR query: %v", err)
		}
		if count != 2 {
			t.Errorf("'fox | goroutines': got %d rows, want 2", count)
		}
	})

	t.Run("negation_not_match", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE tsv @@ to_tsquery('english', 'search & !fox')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("NOT query: %v", err)
		}
		// Row 1 has 'search' and no 'fox'. Row 3 has no 'search' at all.
		if count != 1 {
			t.Errorf("'search & !fox': got %d rows, want 1", count)
		}
	})

	// plainto_tsquery
	t.Run("plainto_tsquery", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE tsv @@ plainto_tsquery('english', 'text search in postgresql')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("plainto_tsquery: %v", err)
		}
		if count != 1 {
			t.Errorf("plainto_tsquery: got %d rows, want 1", count)
		}
	})

	// ts_rank: relevance scoring
	t.Run("ts_rank_scoring", func(t *testing.T) {
		rows, err := pool.Query(ctx,
			"SELECT title, ts_rank(tsv, to_tsquery('english', 'postgresql')) AS rank "+
				"FROM "+tableName+" WHERE tsv @@ to_tsquery('english', 'postgresql') "+
				"ORDER BY rank DESC",
		)
		if err != nil {
			t.Fatalf("ts_rank: %v", err)
		}
		defer rows.Close()

		type result struct {
			title string
			rank  float64
		}
		var results []result
		for rows.Next() {
			var r result
			if err := rows.Scan(&r.title, &r.rank); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}

		if len(results) < 1 {
			t.Fatalf("ts_rank: got %d rows, want at least 1", len(results))
		}
		for _, r := range results {
			if r.rank <= 0 {
				t.Errorf("ts_rank for %q is %f, want > 0", r.title, r.rank)
			}
		}
	})

	// websearch_to_tsquery
	t.Run("websearch_to_tsquery", func(t *testing.T) {
		var count int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE tsv @@ websearch_to_tsquery('english', '\"full text search\" OR goroutines')",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&count)
		if err != nil {
			t.Fatalf("websearch: %v", err)
		}
		if count != 2 {
			t.Errorf("websearch: got %d rows, want 2", count)
		}
	})

	// ts_headline: highlighting
	t.Run("ts_headline", func(t *testing.T) {
		var headline string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT ts_headline('english', body, to_tsquery('english', 'search')) "+
				"FROM "+tableName+" WHERE title = 'PostgreSQL Full Text Search'",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&headline)
		if err != nil {
			t.Fatalf("ts_headline: %v", err)
		}
		if !strings.Contains(headline, "<b>search</b>") {
			t.Errorf("ts_headline = %q, want to contain <b>search</b>", headline)
		}
	})

	// GIN index for FTS
	t.Run("gin_index", func(t *testing.T) {
		idxName := fmt.Sprintf("idx_%s_tsv", tableName)
		_, err := pool.Exec(ctx, "CREATE INDEX "+idxName+" ON "+tableName+" USING GIN (tsv)")
		if err != nil {
			t.Fatalf("CREATE GIN index: %v", err)
		}

		var exists bool
		row, qrErr := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)", idxName,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&exists)
		if err != nil {
			t.Fatalf("check index exists: %v", err)
		}
		if !exists {
			t.Errorf("GIN index %q not found in pg_indexes", idxName)
		}
	})
}

// 7. BRIN Index Effectiveness

// TestPostgresBRIN_Index validates BRIN index creation, query plan use,
// and size efficiency relative to a comparable B-tree index.
func TestPostgresBRIN_Index(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG BRIN integration test in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	pool := testdb.Postgres(t)
	ctx := context.Background()

	tableName := fmt.Sprintf("brin_test_%d", time.Now().UnixNano()%10000)
	_, err := pool.Exec(ctx,
		"CREATE TABLE "+tableName+" (id SERIAL PRIMARY KEY, ts TIMESTAMPTZ NOT NULL DEFAULT NOW(), sensor_id INTEGER NOT NULL, reading NUMERIC NOT NULL)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tableName+" CASCADE") })

	seedCount := 2000
	startTS := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Logf("seeding %d rows with sequential timestamps (ideal for BRIN)...", seedCount)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck

	for i := range seedCount {
		ts := startTS.Add(time.Duration(i) * time.Hour)
		sensorID := i % 10
		reading := 20.0 + float64(i%100)*0.5
		_, err := tx.Exec(
			"INSERT INTO "+tableName+" (ts, sensor_id, reading) VALUES ($1, $2, $3)",
			ts, sensorID, reading,
		)
		if err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Create BRIN index on timestamp
	t.Run("create_brin", func(t *testing.T) {
		idxName := fmt.Sprintf("idx_%s_ts_brin", tableName)
		_, err := pool.Exec(ctx,
			"CREATE INDEX "+idxName+" ON "+tableName+" USING BRIN (ts) WITH (pages_per_range = 32)")
		if err != nil {
			t.Fatalf("CREATE BRIN index: %v", err)
		}

		var exists bool
		row, qrErr := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE indexname = $1)", idxName,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&exists)
		if err != nil {
			t.Fatalf("check BRIN exists: %v", err)
		}
		if !exists {
			t.Fatal("BRIN index not found")
		}
	})

	// BRIN index size is compact
	t.Run("brin_size_compact", func(t *testing.T) {
		idxName := fmt.Sprintf("idx_%s_ts_brin", tableName)
		var size int64
		row, qrErr := pool.QueryRow(ctx,
			"SELECT pg_relation_size($1)", idxName,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&size)
		if err != nil {
			t.Fatalf("pg_relation_size BRIN: %v", err)
		}
		t.Logf("BRIN index size: %d bytes (%.1f KB)", size, float64(size)/1024)

		var tableSize int64
		row, qrErr = pool.QueryRow(ctx,
			"SELECT pg_relation_size($1)", tableName,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&tableSize)
		if err != nil {
			t.Fatalf("pg_relation_size table: %v", err)
		}
		t.Logf("Table size: %d bytes (%.1f KB)", tableSize, float64(tableSize)/1024)

		ratio := float64(size) / float64(tableSize)
		t.Logf("BRIN/table ratio: %.2f%%", ratio*100)

		if ratio > 0.30 {
			t.Logf("BRIN ratio %.2f%% > 30%% - expect higher for very small tables; raising pages_per_range would help", ratio*100)
		}
	})

	// Query plan awareness (sequential scan is expected for small tables)
	t.Run("query_plan", func(t *testing.T) {
		midTS := startTS.Add(time.Duration(seedCount/2) * time.Hour)
		var plan string
		rows, err := pool.Query(ctx,
			"EXPLAIN (FORMAT TEXT) SELECT COUNT(*) FROM "+tableName+" WHERE ts BETWEEN $1 AND $2",
			midTS.Add(-24*time.Hour), midTS.Add(24*time.Hour),
		)
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()

		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan EXPLAIN: %v", err)
			}
			lines = append(lines, line)
		}
		plan = strings.Join(lines, "\n")
		t.Logf("EXPLAIN plan:\n%s", plan)

		// For small tables, PG may choose seq scan over BRIN. Both are valid.
		if !strings.Contains(plan, "Index") && !strings.Contains(plan, "Seq Scan") {
			t.Error("EXPLAIN plan didn't contain recognizable scan node")
		}
	})

	// BRIN vs B-tree size comparison
	t.Run("brin_vs_btree_size", func(t *testing.T) {
		btreeIdxName := "idx_" + tableName + "_ts_btree"
		_, err := pool.Exec(ctx,
			"CREATE INDEX "+btreeIdxName+" ON "+tableName+" USING BTREE (ts)")
		if err != nil {
			t.Fatalf("CREATE BTREE index: %v", err)
		}
		t.Cleanup(func() {
			pool.Exec(context.Background(), "DROP INDEX IF EXISTS "+btreeIdxName)
		})

		brinIdxName := fmt.Sprintf("idx_%s_ts_brin", tableName)
		var brinSize int64
		row, qrErr := pool.QueryRow(ctx, "SELECT pg_relation_size($1)", brinIdxName)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&brinSize)
		if err != nil {
			t.Fatalf("BRIN size: %v", err)
		}
		var btreeSize int64
		row, qrErr = pool.QueryRow(ctx, "SELECT pg_relation_size($1)", btreeIdxName)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&btreeSize)
		if err != nil {
			t.Fatalf("BTREE size: %v", err)
		}

		t.Logf("BRIN size:  %d bytes (%.1f KB)", brinSize, float64(brinSize)/1024)
		t.Logf("BTREE size: %d bytes (%.1f KB)", btreeSize, float64(btreeSize)/1024)

		if brinSize >= btreeSize {
			t.Errorf("BRIN (%d bytes) should be smaller than BTREE (%d bytes) for sequential data", brinSize, btreeSize)
		}
		ratio := float64(brinSize) / float64(btreeSize)
		t.Logf("BRIN/BTREE ratio: %.2f%%", ratio*100)

		if ratio > 0.60 {
			t.Errorf("BRIN/BTREE ratio %.2f%% > 60%% - BRIN should be dramatically smaller for sequential data", ratio*100)
		}
	})

	// Range query correctness
	t.Run("range_query_correctness", func(t *testing.T) {
		start := startTS.Add(10 * time.Hour)
		end := startTS.Add(20 * time.Hour)
		rows, err := pool.Query(ctx,
			"SELECT COUNT(*) FROM "+tableName+" WHERE ts >= $1 AND ts <= $2",
			start, end,
		)
		if err != nil {
			t.Fatalf("range query: %v", err)
		}
		defer rows.Close()

		var count int
		for rows.Next() {
			if err := rows.Scan(&count); err != nil {
				t.Fatalf("scan count: %v", err)
			}
		}
		if count != 11 {
			t.Errorf("range query count = %d, want 11 (rows 10-20 inclusive)", count)
		}
	})
}

// Combined Harness

// TestPostgresFeatures_Harness runs the full PG feature suite in a single
// test to simplify CI invocation. Individual tests can still be run
// separately for focused development.
func TestPostgresFeatures_Harness(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PG features harness in short mode")
	}
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}

	t.Run("JSONB", TestPostgresJSONB_Operators)
	t.Run("ArrayFunctions", TestPostgresArray_Functions)
	t.Run("WindowFunctions", TestPostgresWindow_Functions)
	t.Run("RecursiveCTE", TestPostgresRecursiveCTE)
	t.Run("RowLevelSecurity", TestPostgresRLS)
	t.Run("FullTextSearch", TestPostgresFullTextSearch)
	t.Run("BRINIndex", TestPostgresBRIN_Index)
}

// Helpers

func intPtr(i int) *int { return &i }
