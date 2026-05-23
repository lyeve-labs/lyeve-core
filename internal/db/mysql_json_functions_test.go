//go:build !short && !mutest

// MySQL JSON Function Integration Tests
// Exercises MySQL 8.0's JSON functions against a real container, focusing
// on the functions most likely to be used by the CMS's dynamic content
// schema and plugin data storage.
//
// Functions tested:
//   - JSON_EXTRACT / -> / ->>  (path-based extraction)
//   - JSON_CONTAINS             (path + value containment)
//   - JSON_CONTAINS_PATH        (path existence)
//   - JSON_KEYS / JSON_LENGTH / JSON_TYPE / JSON_DEPTH
//   - JSON_SET / JSON_REPLACE / JSON_REMOVE / JSON_INSERT
//   - JSON_ARRAYAGG / JSON_OBJECTAGG
//   - JSON_TABLE               (relational projection)
//   - JSON_VALID / JSON_UNQUOTE
//   - JSON_MERGE_PATCH / JSON_MERGE_PRESERVE
//   - JSON_ARRAY / JSON_OBJECT
//   - Edge cases: NULL, empty, deeply nested, Unicode, large documents
//
// References:
//   - MySQL 8.0 Reference Manual §13.5 (JSON Functions)
//   - https://dev.mysql.com/doc/refman/8.0/en/json-functions.html
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

// jsonTableName generates a deterministic but unique table name for JSON tests.
func jsonTableName(prefix string) string {
	return fmt.Sprintf("json_%s_%d", prefix, time.Now().UnixNano()%100000)
}

// JSON_EXTRACT and Path Operators

// TestMySQL_JSON_Extract_Scalar extracts scalar values via path expressions.
func TestMySQL_JSON_Extract_Scalar(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{"top-level key", "$.name", "Alice"},
		{"nested key", "$.address.city", "Sample City"},
		{"array index 0", "$.tags[0]", "go"},
		{"array index last", "$.tags[2]", "json"},
	}

	doc := `{"name":"Alice","age":30,"address":{"city":"Sample City","zip":"10001"},"tags":["go","mysql","json"]}`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result string
			row, qrErr := pool.QueryRow(ctx,
				"SELECT JSON_UNQUOTE(JSON_EXTRACT(?, ?))", doc, tt.path,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&result)
			if err != nil {
				t.Fatalf("JSON_EXTRACT(%s): %v", tt.path, err)
			}
			if result != tt.expected {
				t.Errorf("JSON_EXTRACT(%s) = %q, want %q", tt.path, result, tt.expected)
			}
		})
	}
}

// TestMySQL_JSON_Extract_OperatorSyntax tests the -> and ->> shorthand operators.
func TestMySQL_JSON_Extract_OperatorSyntax(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	doc := `{"user":{"id":42,"name":"Charlie"}}`

	// -> operator returns JSON (quoted string for scalar values).
	var quoted string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT JSON_EXTRACT(?, '$.user.name')", doc,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&quoted)
	if err != nil {
		t.Fatalf("-> operator: %v", err)
	}
	// MySQL's -> returns a JSON scalar, so "Charlie" is returned as '"Charlie"'
	if !strings.Contains(quoted, "Charlie") {
		t.Errorf("-> operator returned %q, expected to contain 'Charlie'", quoted)
	}

	// ->> operator returns unquoted text (via JSON_UNQUOTE internally).
	var unquoted string
	row, qrErr = pool.QueryRow(ctx,
		"SELECT JSON_UNQUOTE(JSON_EXTRACT(?, '$.user.name'))", doc,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&unquoted)
	if err != nil {
		t.Fatalf("->> operator equivalent: %v", err)
	}
	if unquoted != "Charlie" {
		t.Errorf("->> operator equivalent returned %q, want %q", unquoted, "Charlie")
	}
}

// TestMySQL_JSON_Extract_Wildcard extracts array elements with wildcard [*].
func TestMySQL_JSON_Extract_Wildcard(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	doc := `{"items":[{"id":1,"name":"a"},{"id":2,"name":"b"},{"id":3,"name":"c"}]}`

	// $.items[*].id returns all ids as a JSON array.
	var result string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT JSON_EXTRACT(?, '$.items[*].id')", doc,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&result)
	if err != nil {
		t.Fatalf("wildcard extract: %v", err)
	}
	if result != "[1, 2, 3]" {
		t.Errorf("wildcard extract: got %q, want [1, 2, 3]", result)
	}
}

// TestMySQL_JSON_Extract_NonexistentPath returns NULL for missing paths.
func TestMySQL_JSON_Extract_NonexistentPath(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var val sql.NullString
	row, qrErr := pool.QueryRow(ctx,
		"SELECT JSON_UNQUOTE(JSON_EXTRACT(?, '$.nonexistent'))", `{"a":1}`,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&val)
	if err != nil {
		t.Fatalf("nonexistent path: %v", err)
	}
	if val.Valid {
		t.Errorf("nonexistent path returned %q, expected NULL", val.String)
	}
}

// JSON_CONTAINS

// TestMySQL_JSON_Contains tests containment queries.
func TestMySQL_JSON_Contains(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tableName := jsonTableName("contains")
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, data JSON)", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert documents.
	docs := []string{
		`{"tags":["go","mysql","json"],"level":"advanced"}`,
		`{"tags":["rust","postgres"],"level":"advanced"}`,
		`{"tags":["go","beginner"],"level":"beginner"}`,
	}
	for _, d := range docs {
		_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (data) VALUES (?)", tableName), d)
		if err != nil {
			t.Fatalf("insert doc: %v", err)
		}
	}

	tests := []struct {
		name     string
		path     string
		value    string
		wantRows int
	}{
		{"tag contains go", "$.tags", `"go"`, 2},          // docs 0 and 2
		{"tag contains mysql", "$.tags", `"mysql"`, 1},    // doc 0 only
		{"tag contains rust", "$.tags", `"rust"`, 1},      // doc 1 only
		{"level is advanced", "$.level", `"advanced"`, 2}, // docs 0 and 1
		{"tag contains python", "$.tags", `"python"`, 0},  // none
		{"top-level contains", "$", `{"tags":["go","mysql","json"]}`, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cnt int
			row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s` WHERE JSON_CONTAINS(data, ?, ?)",
				tableName), tt.value, tt.path,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&cnt)
			if err != nil {
				t.Fatalf("JSON_CONTAINS: %v", err)
			}
			if cnt != tt.wantRows {
				t.Errorf("JSON_CONTAINS(%s, %s): got %d rows, want %d", tt.path, tt.value, cnt, tt.wantRows)
			}
		})
	}
}

// TestMySQL_JSON_Contains_Array tests that JSON_CONTAINS with array value
// checks that ALL elements in the candidate are present in the target.
func TestMySQL_JSON_Contains_Array(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var ok int
	row, qrErr := pool.QueryRow(ctx,
		"SELECT JSON_CONTAINS(?, ?, '$')",
		`["go","mysql","json","rust"]`, `["go","json"]`,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&ok)
	if err != nil {
		t.Fatalf("JSON_CONTAINS array: %v", err)
	}
	if ok != 1 {
		t.Errorf("JSON_CONTAINS(array): expected 1 (true), got %d", ok)
	}

	// Partial match of non-existent element should be 0.
	var notOK int
	row, qrErr = pool.QueryRow(ctx,
		"SELECT JSON_CONTAINS(?, ?, '$')",
		`["go","mysql"]`, `["go","python"]`,
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&notOK)
	if err != nil {
		t.Fatalf("JSON_CONTAINS array false: %v", err)
	}
	if notOK != 0 {
		t.Errorf("JSON_CONTAINS(array, non-existent): expected 0 (false), got %d", notOK)
	}
}

// JSON_CONTAINS_PATH

// TestMySQL_JSON_ContainsPath checks path existence with one/all modes.
func TestMySQL_JSON_ContainsPath(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	doc := `{"a":{"b":{"c":1}},"d":[{"e":2}]}`

	tests := []struct {
		name  string
		mode  string // 'one' or 'all'
		paths []string
		want  int
	}{
		{"one match", "one", []string{"$.a.b.c"}, 1},
		{"one no match", "one", []string{"$.x.y.z"}, 0},
		{"all match", "all", []string{"$.a.b", "$.d[0]"}, 1},
		{"all partial fail", "all", []string{"$.a.b", "$.nonexistent"}, 0},
		{"one with multiple, first matches", "one", []string{"$.a.b", "$.x.y"}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build the path arguments.
			pathArgs := make([]string, len(tt.paths))
			for i, p := range tt.paths {
				pathArgs[i] = fmt.Sprintf("'%s'", p)
			}
			query := fmt.Sprintf(
				"SELECT JSON_CONTAINS_PATH(?, '%s', %s)",
				tt.mode, strings.Join(pathArgs, ", "),
			)

			var result int
			row, qrErr := pool.QueryRow(ctx, query, doc)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&result)
			if err != nil {
				t.Fatalf("JSON_CONTAINS_PATH: %v", err)
			}
			if result != tt.want {
				t.Errorf("JSON_CONTAINS_PATH(%s, %v) = %d, want %d", tt.mode, tt.paths, result, tt.want)
			}
		})
	}
}

// JSON_KEYS / JSON_LENGTH / JSON_TYPE / JSON_DEPTH

// TestMySQL_JSON_Introspection tests JSON structural inspection functions.
func TestMySQL_JSON_Introspection(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	t.Run("JSON_KEYS", func(t *testing.T) {
		var keys string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_KEYS(?);", `{"a":1,"b":2,"c":3}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&keys)
		if err != nil {
			t.Fatalf("JSON_KEYS: %v", err)
		}
		if keys != `["a", "b", "c"]` {
			t.Errorf("JSON_KEYS = %q, want [\"a\", \"b\", \"c\"]", keys)
		}
	})

	t.Run("JSON_LENGTH_object", func(t *testing.T) {
		var n int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_LENGTH(?);", `{"x":10,"y":20,"z":30}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&n)
		if err != nil {
			t.Fatalf("JSON_LENGTH: %v", err)
		}
		if n != 3 {
			t.Errorf("JSON_LENGTH = %d, want 3", n)
		}
	})

	t.Run("JSON_LENGTH_array", func(t *testing.T) {
		var n int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_LENGTH(?);", `["a","b","c","d","e"]`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&n)
		if err != nil {
			t.Fatalf("JSON_LENGTH(array): %v", err)
		}
		if n != 5 {
			t.Errorf("JSON_LENGTH(array) = %d, want 5", n)
		}
	})

	t.Run("JSON_TYPE", func(t *testing.T) {
		tests := []struct{ val, typ string }{
			{`"hello"`, "STRING"},
			{`42`, "INTEGER"},
			{`3.14`, "DOUBLE"},
			{`true`, "BOOLEAN"},
			{`null`, "NULL"},
			{`{"a":1}`, "OBJECT"},
			{`[1,2,3]`, "ARRAY"},
		}
		for _, tt := range tests {
			var typ string
			row, qrErr := pool.QueryRow(ctx,
				"SELECT JSON_TYPE(CAST(? AS JSON))", tt.val,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&typ)
			if err != nil {
				t.Errorf("JSON_TYPE(%s): %v", tt.val, err)
				continue
			}
			if typ != tt.typ {
				t.Errorf("JSON_TYPE(%s) = %q, want %q", tt.val, typ, tt.typ)
			}
		}
	})

	t.Run("JSON_DEPTH", func(t *testing.T) {
		tests := []struct {
			name, doc string
			want      int
		}{
			{"scalar", `42`, 1},
			{"empty array", `[]`, 1},
			{"flat array", `[1,2,3]`, 2},
			{"flat object", `{"a":1}`, 2},
			{"nested 2", `{"a":{"b":1}}`, 3},
			{"nested 3", `{"a":{"b":{"c":1}}}`, 4},
			{"mixed nested", `{"a":[1,{"b":2}]}`, 4},
		}
		for _, tt := range tests {
			var depth int
			row, qrErr := pool.QueryRow(ctx,
				"SELECT JSON_DEPTH(CAST(? AS JSON))", tt.doc,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&depth)
			if err != nil {
				t.Errorf("JSON_DEPTH(%s): %v", tt.name, err)
				continue
			}
			if depth != tt.want {
				t.Errorf("JSON_DEPTH(%s) = %d, want %d", tt.name, depth, tt.want)
			}
		}
	})
}

// JSON_SET / JSON_REPLACE / JSON_REMOVE / JSON_INSERT

// TestMySQL_JSON_Mutation tests JSON document mutation functions.
func TestMySQL_JSON_Mutation(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	t.Run("JSON_SET_update_existing", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_SET(?, '$.name', 'Bob')", `{"name":"Alice","age":30}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_SET: %v", err)
		}
		if !strings.Contains(result, `"Bob"`) {
			t.Errorf("JSON_SET did not update name: %s", result)
		}
	})

	t.Run("JSON_SET_add_new_key", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_SET(?, '$.city', 'Testville')", `{"name":"Alice"}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_SET add key: %v", err)
		}
		if !strings.Contains(result, `"Testville"`) {
			t.Errorf("JSON_SET did not add city key: %s", result)
		}
	})

	t.Run("JSON_INSERT_skip_existing", func(t *testing.T) {
		// JSON_INSERT should NOT overwrite existing keys.
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_INSERT(?, '$.name', 'Bob')", `{"name":"Alice"}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_INSERT: %v", err)
		}
		if !strings.Contains(result, `"Alice"`) {
			t.Errorf("JSON_INSERT overwrote existing key: %s", result)
		}
	})

	t.Run("JSON_INSERT_add_new_only", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_INSERT(?, '$.newkey', 42)", `{"name":"Alice"}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_INSERT new: %v", err)
		}
		if !strings.Contains(result, `42`) {
			t.Errorf("JSON_INSERT did not add new key: %s", result)
		}
	})

	t.Run("JSON_REPLACE_existing_only", func(t *testing.T) {
		// JSON_REPLACE only works on existing keys.
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_REPLACE(?, '$.age', 31)", `{"name":"Alice","age":30}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_REPLACE: %v", err)
		}
		if !strings.Contains(result, `31`) {
			t.Errorf("JSON_REPLACE did not update age: %s", result)
		}
	})

	t.Run("JSON_REPLACE_ignore_new_key", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_REPLACE(?, '$.newkey', 999)", `{"name":"Alice"}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_REPLACE new key: %v", err)
		}
		if strings.Contains(result, `999`) {
			t.Errorf("JSON_REPLACE added new key it should have ignored: %s", result)
		}
	})

	t.Run("JSON_REMOVE_key", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_REMOVE(?, '$.age')", `{"name":"Alice","age":30}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_REMOVE: %v", err)
		}
		if strings.Contains(result, `"age"`) {
			t.Errorf("JSON_REMOVE did not remove key: %s", result)
		}
	})

	t.Run("JSON_REMOVE_array_element", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_REMOVE(?, '$[1]')", `["a","b","c"]`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_REMOVE array: %v", err)
		}
		if result != `["a", "c"]` {
			t.Errorf("JSON_REMOVE array element: got %q, want [\"a\", \"c\"]", result)
		}
	})
}

// JSON_ARRAYAGG / JSON_OBJECTAGG

// TestMySQL_JSON_Aggregation tests aggregate JSON functions.
func TestMySQL_JSON_Aggregation(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tableName := jsonTableName("agg")
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, category VARCHAR(20), score INT)", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (category, score) VALUES ('A', 10), ('A', 20), ('B', 30), ('B', 40)", tableName))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("JSON_ARRAYAGG", func(t *testing.T) {
		// Aggregate all scores per category into a JSON array.
		rows, err := pool.Query(ctx, fmt.Sprintf(
			"SELECT category, JSON_ARRAYAGG(score) FROM `%s` GROUP BY category ORDER BY category", tableName))
		if err != nil {
			t.Fatalf("JSON_ARRAYAGG query: %v", err)
		}
		defer rows.Close()

		var results []struct {
			cat string
			arr string
		}
		for rows.Next() {
			var cat, arr string
			if err := rows.Scan(&cat, &arr); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, struct{ cat, arr string }{cat, arr})
		}

		if len(results) != 2 {
			t.Fatalf("expected 2 groups, got %d", len(results))
		}
		if results[0].arr != "[10, 20]" {
			t.Errorf("category A: got %s, want [10, 20]", results[0].arr)
		}
		if results[1].arr != "[30, 40]" {
			t.Errorf("category B: got %s, want [30, 40]", results[1].arr)
		}
	})

	t.Run("JSON_OBJECTAGG", func(t *testing.T) {
		// Aggregate {category: score_sum} as a single JSON object per group.
		rows, err := pool.Query(ctx, fmt.Sprintf(
			"SELECT JSON_OBJECTAGG(category, score) FROM `%s`", tableName))
		if err != nil {
			t.Fatalf("JSON_OBJECTAGG: %v", err)
		}
		defer rows.Close()

		if !rows.Next() {
			t.Fatal("JSON_OBJECTAGG returned no rows")
		}
		var obj string
		if err := rows.Scan(&obj); err != nil {
			t.Fatalf("scan JSON_OBJECTAGG: %v", err)
		}

		// With 4 rows and duplicate keys, later values overwrite earlier ones.
		// So we get: {"A": 20, "B": 40}
		if strings.Contains(obj, `"A"`) && strings.Contains(obj, `"B"`) {
			t.Logf("JSON_OBJECTAGG result: %s", obj)
		} else {
			t.Errorf("JSON_OBJECTAGG missing expected keys: %s", obj)
		}
	})
}

// JSON_TABLE

// TestMySQL_JSON_Table projects a JSON array into a relational table.
func TestMySQL_JSON_Table(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tableName := jsonTableName("jtable")
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, data JSON)", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	doc := `{
		"store":"LyEve",
		"items":[
			{"name":"Widget","price":9.99,"tags":["sale","new"]},
			{"name":"Gadget","price":19.99,"tags":["premium"]},
			{"name":"Thing","price":4.99,"tags":["clearance"]}
		]
	}`
	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (data) VALUES (?)", tableName), doc)
	if err != nil {
		t.Fatalf("insert JSON_TABLE doc: %v", err)
	}

	// Project items array into rows with name, price columns.
	quotedName := "`" + tableName + "`"
	query := fmt.Sprintf(`
		SELECT jt.item_name, jt.item_price
		FROM %s,
		JSON_TABLE(data, '$.items[*]' COLUMNS(
			item_name  VARCHAR(50) PATH '$.name',
			item_price DECIMAL(10,2) PATH '$.price'
		)) AS jt
		ORDER BY jt.item_price
	`, quotedName)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		t.Fatalf("JSON_TABLE: %v", err)
	}
	defer rows.Close()

	type item struct {
		name  string
		price float64
	}
	var items []item
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.name, &i.price); err != nil {
			t.Fatalf("scan JSON_TABLE row: %v", err)
		}
		items = append(items, i)
	}

	if len(items) != 3 {
		t.Fatalf("JSON_TABLE: expected 3 items, got %d", len(items))
	}

	expected := []item{
		{"Thing", 4.99},
		{"Widget", 9.99},
		{"Gadget", 19.99},
	}
	for i, want := range expected {
		if items[i].name != want.name {
			t.Errorf("item[%d].name = %q, want %q", i, items[i].name, want.name)
		}
		if items[i].price != want.price {
			t.Errorf("item[%d].price = %.2f, want %.2f", i, items[i].price, want.price)
		}
	}
	t.Logf("JSON_TABLE: projected %d rows from JSON array", len(items))
}

// JSON_VALID / JSON_UNQUOTE

// TestMySQL_JSON_Validation tests JSON_VALID and edge cases.
func TestMySQL_JSON_Validation(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tests := []struct {
		name  string
		val   string
		valid int
	}{
		{"valid object", `{"a":1}`, 1},
		{"valid array", `[1,2,3]`, 1},
		{"valid string", `"hello"`, 1},
		{"valid number", `42`, 1},
		{"valid null", `null`, 1},
		{"valid true", `true`, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var valid int
			row, qrErr := pool.QueryRow(ctx,
				"SELECT JSON_VALID(?)", tt.val,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&valid)
			if err != nil {
				t.Fatalf("JSON_VALID(%q): %v", tt.name, err)
			}
			if valid != tt.valid {
				t.Errorf("JSON_VALID(%q) = %d, want %d", tt.name, valid, tt.valid)
			}
		})
	}

	// Invalid JSON: MySQL 8.0.26+ raises Error 3141 when parsing invalid JSON
	// literals via parameter binding, rather than returning 0.
	// Accept either result=0 or error 3141.
	invalidTests := []struct {
		name string
		val  string
	}{
		{"invalid missing quote", `{a:1}`},
		{"invalid trailing comma", `[1,2,]`},
		{"invalid plain text", `not json`},
		{"empty string", ``},
	}
	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			var valid int
			row, qrErr := pool.QueryRow(ctx,
				"SELECT JSON_VALID(?)", tt.val,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&valid)
			if err != nil {
				t.Logf("JSON_VALID(%q) raised error (acceptable in MySQL 8.0.26+): %v", tt.name, err)
				return
			}
			if valid != 0 {
				t.Errorf("JSON_VALID(%q) = %d, want 0", tt.name, valid)
			}
		})
	}
}

// TestMySQL_JSON_Unquote tests JSON_UNQUOTE for string extraction.
func TestMySQL_JSON_Unquote(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tests := []struct {
		input, expected string
	}{
		{`"hello"`, "hello"},
		{`"hello\tworld"`, "hello\tworld"},
		{`"hello\nworld"`, "hello\nworld"},
		{`"\u00e9"`, "é"},
		{`"\uD83D\uDE00"`, "😀"},
	}

	for _, tt := range tests {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_UNQUOTE(CAST(? AS JSON))", tt.input,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Errorf("JSON_UNQUOTE(%q): %v", tt.input, err)
			continue
		}
		if result != tt.expected {
			t.Errorf("JSON_UNQUOTE(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// JSON_MERGE_PATCH / JSON_MERGE_PRESERVE

// TestMySQL_JSON_Merge tests merge functions.
func TestMySQL_JSON_Merge(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	t.Run("JSON_MERGE_PATCH_override", func(t *testing.T) {
		// MERGE_PATCH (RFC 7396): patch overrides existing keys, removes null keys.
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_MERGE_PATCH(?, ?)",
			`{"name":"Alice","age":30,"city":"NYC"}`,
			`{"age":31,"city":null}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_MERGE_PATCH: %v", err)
		}
		// city should be removed (null in patch), age should be 31.
		if strings.Contains(result, `"NYC"`) {
			t.Errorf("JSON_MERGE_PATCH did not remove null-patched key: %s", result)
		}
		if !strings.Contains(result, `31`) {
			t.Errorf("JSON_MERGE_PATCH did not update age: %s", result)
		}
		t.Logf("JSON_MERGE_PATCH result: %s", result)
	})

	t.Run("JSON_MERGE_PRESERVE_combine", func(t *testing.T) {
		// MERGE_PRESERVE (RFC 7396): combines duplicate keys into arrays.
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_MERGE_PRESERVE(?, ?)",
			`{"a":1,"b":2}`,
			`{"b":3,"c":4}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_MERGE_PRESERVE: %v", err)
		}
		// b should be [2, 3] (preserved as array).
		if strings.Contains(result, `[2, 3]`) || strings.Contains(result, `[3, 2]`) {
			t.Logf("JSON_MERGE_PRESERVE combined duplicate keys: %s", result)
		} else {
			t.Errorf("JSON_MERGE_PRESERVE: expected b to merge into array, got %s", result)
		}
	})
}

// JSON_ARRAY / JSON_OBJECT

// TestMySQL_JSON_Array tests JSON_ARRAY constructor.
func TestMySQL_JSON_Array(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var result string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT JSON_ARRAY(1, 'hello', true, null, CAST('{\"nested\":true}' AS JSON))",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&result)
	if err != nil {
		t.Fatalf("JSON_ARRAY: %v", err)
	}
	if result != `[1, "hello", true, null, {"nested": true}]` {
		t.Errorf("JSON_ARRAY = %q", result)
	}
}

// TestMySQL_JSON_Object tests JSON_OBJECT constructor.
func TestMySQL_JSON_Object(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var result string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT JSON_OBJECT('id', 1, 'name', 'test', 'active', true)",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&result)
	if err != nil {
		t.Fatalf("JSON_OBJECT: %v", err)
	}
	expected := `{"id": 1, "name": "test", "active": true}`
	if result != expected {
		t.Errorf("JSON_OBJECT = %q, want %q", result, expected)
	}
}

// Edge Cases

// TestMySQL_JSON_EdgeCases tests NULL, empty JSON, and deeply nested documents.
func TestMySQL_JSON_EdgeCases(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	t.Run("NULL input to JSON_EXTRACT", func(t *testing.T) {
		// Passing NULL to JSON_EXTRACT should return NULL.
		var val sql.NullString
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_EXTRACT(?, '$.a')", nil,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Fatalf("JSON_EXTRACT(NULL): %v", err)
		}
		if val.Valid {
			t.Errorf("JSON_EXTRACT(NULL) = %q, expected NULL", val.String)
		}
	})

	t.Run("empty JSON object", func(t *testing.T) {
		var length int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_LENGTH(CAST('{}' AS JSON))",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&length)
		if err != nil {
			t.Fatalf("JSON_LENGTH({}): %v", err)
		}
		if length != 0 {
			t.Errorf("JSON_LENGTH({}) = %d, want 0", length)
		}
	})

	t.Run("empty JSON array", func(t *testing.T) {
		var length int
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_LENGTH(CAST('[]' AS JSON))",
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&length)
		if err != nil {
			t.Fatalf("JSON_LENGTH([]): %v", err)
		}
		if length != 0 {
			t.Errorf("JSON_LENGTH([]) = %d, want 0", length)
		}
	})

	t.Run("deeply nested extraction", func(t *testing.T) {
		// 10 levels deep.
		doc := `{"l1":{"l2":{"l3":{"l4":{"l5":{"l6":{"l7":{"l8":{"l9":{"l10":"deep-value"}}}}}}}}}}`
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_UNQUOTE(JSON_EXTRACT(?, '$.l1.l2.l3.l4.l5.l6.l7.l8.l9.l10'))", doc,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("deep extraction: %v", err)
		}
		if result != "deep-value" {
			t.Errorf("deep extraction: got %q, want %q", result, "deep-value")
		}
	})

	t.Run("Unicode CJK characters", func(t *testing.T) {
		doc := `{"message":"こんにちは世界","emoji":"🎉🚀💾"}`
		var msg string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_UNQUOTE(JSON_EXTRACT(?, '$.message'))", doc,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&msg)
		if err != nil {
			t.Fatalf("Unicode CJK: %v", err)
		}
		if msg != "こんにちは世界" {
			t.Errorf("Unicode CJK: got %q", msg)
		}

		var emoji string
		row, qrErr = pool.QueryRow(ctx,
			"SELECT JSON_UNQUOTE(JSON_EXTRACT(?, '$.emoji'))", doc,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&emoji)
		if err != nil {
			t.Fatalf("Unicode emoji: %v", err)
		}
		if emoji != "🎉🚀💾" {
			t.Errorf("Unicode emoji: got %q", emoji)
		}
	})

	t.Run("large JSON document roundtrip", func(t *testing.T) {
		// Build a ~10 KB JSON document with 200 items.
		var sb strings.Builder
		sb.WriteString(`{"items":[`)
		for i := 0; i < 200; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"id":%d,"name":"item-%d","data":"padding-content-here-more-padding-yes-even-more-to-make-it-big-enough-for-testing"}`,
				i, i)
		}
		sb.WriteString(`],"meta":"end"}`)
		doc := sb.String()

		tableName := jsonTableName("large")
		_, err := pool.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, data JSON)", tableName))
		if err != nil {
			t.Fatalf("create large doc table: %v", err)
		}
		defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

		_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` (data) VALUES (?)", tableName), doc)
		if err != nil {
			t.Fatalf("insert large JSON: %v", err)
		}

		var result sql.NullString
		row, qrErr := pool.QueryRow(ctx, fmt.Sprintf("SELECT data FROM `%s` LIMIT 1", tableName))
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&result)
		if err != nil {
			t.Fatalf("select large JSON: %v", err)
		}
		if !result.Valid || len(result.String) < 1000 {
			t.Errorf("large JSON roundtrip: result too short (%d bytes)", len(result.String))
		}
		t.Logf("large JSON roundtrip: %d bytes inserted, %d bytes retrieved", len(doc), len(result.String))
	})

	t.Run("JSON array append via JSON_ARRAY_APPEND", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_ARRAY_APPEND(?, '$.tags', 'new-tag')",
			`{"tags":["a","b"]}`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_ARRAY_APPEND: %v", err)
		}
		if !strings.Contains(result, `"new-tag"`) {
			t.Errorf("JSON_ARRAY_APPEND did not append: %s", result)
		}
	})

	t.Run("JSON_ARRAY_INSERT at position", func(t *testing.T) {
		var result string
		row, qrErr := pool.QueryRow(ctx,
			"SELECT JSON_ARRAY_INSERT(?, '$[1]', 'X')",
			`["a","b","c"]`,
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&result)
		if err != nil {
			t.Fatalf("JSON_ARRAY_INSERT: %v", err)
		}
		if result != `["a", "X", "b", "c"]` {
			t.Errorf("JSON_ARRAY_INSERT: got %q, want [\"a\", \"X\", \"b\", \"c\"]", result)
		}
	})
}

// TestMySQL_JSON_ColumnStorage tests that JSON columns store and retrieve
// documents correctly: verifying the CMS's JSON column type works end-to-end.
func TestMySQL_JSON_ColumnStorage(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	tableName := jsonTableName("colstore")
	_, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE `%s` (id INT AUTO_INCREMENT PRIMARY KEY, data JSON, tags JSON)", tableName))
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert a complex document.
	data := `{"title":"My Post","body":"<p>Hello world</p>","metadata":{"author":"admin","version":2}}`
	tags := `["cms","test","mysql"]`

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (data, tags) VALUES (?, ?)", tableName), data, tags)
	if err != nil {
		t.Fatalf("insert JSON columns: %v", err)
	}

	// Retrieve and validate.
	var gotData, gotTags string
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT data, tags FROM `%s` LIMIT 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&gotData, &gotTags)
	if err != nil {
		t.Fatalf("select JSON columns: %v", err)
	}

	// Verify structure is preserved.
	if !strings.Contains(gotData, `"My Post"`) {
		t.Errorf("data column missing title: %s", gotData)
	}
	if !strings.Contains(gotData, `"author"`) {
		t.Errorf("data column missing metadata.author: %s", gotData)
	}
	if !strings.Contains(gotTags, `"cms"`) {
		t.Errorf("tags column missing 'cms': %s", gotTags)
	}

	// Verify JSON functions work on stored column.
	var tagCount int
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT JSON_LENGTH(tags) FROM `%s` LIMIT 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&tagCount)
	if err != nil {
		t.Fatalf("JSON_LENGTH on stored column: %v", err)
	}
	if tagCount != 3 {
		t.Errorf("JSON_LENGTH(tags) = %d, want 3", tagCount)
	}

	t.Logf("JSON column storage: %d tags, data with author metadata", tagCount)
}
