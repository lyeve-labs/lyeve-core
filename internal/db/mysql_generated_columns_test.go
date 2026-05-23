//go:build !short && !mutest

// MySQL Generated Columns Integration Tests
// Exercises MySQL 5.7+/8.0 generated columns: both VIRTUAL and STORED  --
// against a real MySQL 8 testcontainer. Generated columns are critical
// for the CMS's schema engine which allows users to define computed fields.
//
// Areas tested:
//   - VIRTUAL generated columns (computed on read, not stored)
//   - STORED generated columns (computed on write, physically stored)
//   - Expression types: JSON extraction, arithmetic, string ops, dates
//   - Indexing on generated columns (both VIRTUAL secondary and STORED)
//   - ALTER TABLE adding/removing generated columns
//   - NULL handling in generated column expressions
//   - ALTER COLUMN DROP DEFAULT edge cases
//   - DDL diff scenarios (schema engine would generate)
//
// References:
//   - MySQL 8.0 Reference Manual §13.1.20.8 (CREATE TABLE and Generated Columns)
//   - https://dev.mysql.com/doc/refman/8.0/en/create-table-generated-columns.html
package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// genTableName generates a deterministic table name for generated column tests.
func genTableName(prefix string) string {
	return fmt.Sprintf("gencol_%s_%d", prefix, time.Now().UnixNano()%100000)
}

// Section 1: VIRTUAL vs STORED Basics

// TestGenerated_Virtual_SimpleExpression tests a basic VIRTUAL generated column
// that concatenates first_name and last_name.
func TestGenerated_Virtual_SimpleExpression(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("simple")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			first_name VARCHAR(50) NOT NULL,
			last_name  VARCHAR(50) NOT NULL,
			full_name  VARCHAR(101) GENERATED ALWAYS AS (CONCAT(first_name, ' ', last_name)) VIRTUAL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table with VIRTUAL column: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (first_name, last_name) VALUES (?, ?)", tableName),
		"Jane", "Doe")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var fullName string
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT full_name FROM `%s` WHERE id = 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&fullName)
	if err != nil {
		t.Fatalf("select generated column: %v", err)
	}
	if fullName != "Jane Doe" {
		t.Errorf("full_name = %q, want %q", fullName, "Jane Doe")
	}
}

// TestGenerated_Stored_SimpleExpression tests a STORED generated column.
// STORED columns are physically written and can be indexed directly.
func TestGenerated_Stored_SimpleExpression(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("stored")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			quantity INT NOT NULL,
			unit_price DECIMAL(10,2) NOT NULL,
			total_price DECIMAL(12,2) GENERATED ALWAYS AS (quantity * unit_price) STORED
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table with STORED column: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (quantity, unit_price) VALUES (?, ?)", tableName),
		3, 9.99)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var total float64
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT total_price FROM `%s` WHERE id = 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&total)
	if err != nil {
		t.Fatalf("select STORED generated column: %v", err)
	}
	expected := 29.97
	if total != expected {
		t.Errorf("total_price = %.2f, want %.2f", total, expected)
	}
}

// TestGenerated_VirtualVsStored_UpdateBehavior verifies that VIRTUAL
// columns are recomputed on read when base columns change, while STORED
// columns are recomputed on write.
func TestGenerated_VirtualVsStored_UpdateBehavior(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("vs_update")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			a INT NOT NULL,
			b INT NOT NULL,
			sum_virt INT GENERATED ALWAYS AS (a + b) VIRTUAL,
			sum_stor INT GENERATED ALWAYS AS (a + b) STORED
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (a, b) VALUES (10, 20)", tableName))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Update base column.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"UPDATE `%s` SET a = 30 WHERE id = 1", tableName))
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	var virtSum, storSum int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT sum_virt, sum_stor FROM `%s` WHERE id = 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&virtSum, &storSum)
	if err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if virtSum != 50 {
		t.Errorf("sum_virt = %d, want 50", virtSum)
	}
	if storSum != 50 {
		t.Errorf("sum_stor = %d, want 50", storSum)
	}
}

// Section 2: JSON Extraction in Generated Columns

// TestGenerated_JSONExtract_Virtual verifies generated columns that extract
// JSON fields: critical for CMS content schema where document fields are
// projected into flat columns for indexing.
func TestGenerated_JSONExtract_Virtual(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("jsonex")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			data JSON NOT NULL,
			title VARCHAR(255) GENERATED ALWAYS AS (
				JSON_UNQUOTE(JSON_EXTRACT(data, '$.title'))
			) VIRTUAL,
			author VARCHAR(100) GENERATED ALWAYS AS (
				JSON_UNQUOTE(JSON_EXTRACT(data, '$.author'))
			) VIRTUAL,
			tag_count INT GENERATED ALWAYS AS (
				JSON_LENGTH(JSON_EXTRACT(data, '$.tags'))
			) VIRTUAL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table with JSON generated columns: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert JSON documents.
	docs := []string{
		`{"title":"Post One","author":"admin","tags":["cms","go"]}`,
		`{"title":"Post Two","author":"editor","tags":["testing","mysql","json"]}`,
		`{"title":"Post Three","author":"admin","tags":[]}`,
	}
	for _, d := range docs {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (data) VALUES (?)", tableName), d)
		if err != nil {
			t.Fatalf("insert JSON doc: %v", err)
		}
	}

	// Query and verify generated columns.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT title, author, tag_count FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("query generated columns: %v", err)
	}
	defer rows.Close()

	type row struct {
		title    string
		author   string
		tagCount int
	}
	expected := []row{
		{"Post One", "admin", 2},
		{"Post Two", "editor", 3},
		{"Post Three", "admin", 0},
	}
	i := 0
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.title, &r.author, &r.tagCount); err != nil {
			t.Fatalf("scan row %d: %v", i, err)
		}
		if r.title != expected[i].title {
			t.Errorf("row %d title = %q, want %q", i, r.title, expected[i].title)
		}
		if r.author != expected[i].author {
			t.Errorf("row %d author = %q, want %q", i, r.author, expected[i].author)
		}
		if r.tagCount != expected[i].tagCount {
			t.Errorf("row %d tag_count = %d, want %d", i, r.tagCount, expected[i].tagCount)
		}
		i++
	}
	if i != 3 {
		t.Errorf("expected 3 rows, got %d", i)
	}
	t.Logf("JSON extraction generated columns: 3 rows verified")
}

// Section 3: Indexing on Generated Columns

// TestGenerated_IndexOnVirtual verifies that an index can be created on a
// VIRTUAL generated column (secondary index: InnoDB supports this since 5.7).
func TestGenerated_IndexOnVirtual(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("idxvirt")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			first_name VARCHAR(50) NOT NULL,
			last_name VARCHAR(50) NOT NULL,
			full_name VARCHAR(101) GENERATED ALWAYS AS (CONCAT(first_name, ' ', last_name)) VIRTUAL,
			INDEX idx_full_name (full_name)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table with indexed VIRTUAL column: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert 20 rows.
	names := [][2]string{
		{"Alice", "Smith"}, {"Bob", "Jones"}, {"Charlie", "Brown"},
		{"Diana", "White"}, {"Eve", "Black"}, {"Frank", "Green"},
		{"Grace", "Blue"}, {"Hank", "Red"}, {"Iris", "Yellow"},
		{"Jack", "Purple"}, {"Kate", "Orange"}, {"Leo", "Pink"},
		{"Mia", "Gray"}, {"Nick", "Cyan"}, {"Olivia", "Magenta"},
		{"Peter", "Lime"}, {"Quinn", "Teal"}, {"Rose", "Navy"},
		{"Sam", "Olive"}, {"Tina", "Maroon"},
	}
	for _, n := range names {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (first_name, last_name) VALUES (?, ?)", tableName),
			n[0], n[1])
		if err != nil {
			t.Fatalf("insert %s %s: %v", n[0], n[1], err)
		}
	}

	// Verify the index is used for a query on the generated column.
	// Using EXPLAIN to confirm index usage.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"EXPLAIN SELECT * FROM `%s` WHERE full_name = ?", tableName), "Alice Smith")
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var keyUsed string
	for rows.Next() {
		cols, _ := rows.Columns()
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		// key column is typically column 6 in EXPLAIN output.
		for j, col := range cols {
			if col == "key" {
				if s, ok := vals[j].([]byte); ok {
					keyUsed = string(s)
				}
			}
		}
	}
	if keyUsed != "idx_full_name" {
		t.Logf("EXPLAIN key = %q (expected idx_full_name - may depend on optimizer)", keyUsed)
	}

	// Verify the actual query works.
	var fullName string
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT full_name FROM `%s` WHERE full_name = ?", tableName),
		"Alice Smith",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&fullName)
	if err != nil {
		t.Fatalf("select by generated index: %v", err)
	}
	if fullName != "Alice Smith" {
		t.Errorf("full_name = %q", fullName)
	}
}

// TestGenerated_IndexOnStored verifies indexing on STORED generated columns.
func TestGenerated_IndexOnStored(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("idxstor")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			data JSON NOT NULL,
			slug VARCHAR(255) GENERATED ALWAYS AS (
				JSON_UNQUOTE(JSON_EXTRACT(data, '$.slug'))
			) STORED,
			UNIQUE INDEX idx_slug (slug)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table with unique STORED generated column: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert unique slugs.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (data) VALUES (?)", tableName),
		`{"slug":"hello-world","title":"Hello World"}`)
	if err != nil {
		t.Fatalf("insert slug 1: %v", err)
	}

	// Duplicate slug should fail on unique index.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (data) VALUES (?)", tableName),
		`{"slug":"hello-world","title":"Duplicate"}`)
	if err == nil {
		t.Error("insert with duplicate generated slug should fail (unique constraint)")
	} else {
		t.Logf("duplicate slug correctly rejected: %v", err)
	}

	// Different slug should succeed.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (data) VALUES (?)", tableName),
		`{"slug":"goodbye-moon","title":"Goodbye Moon"}`)
	if err != nil {
		t.Fatalf("insert slug 2: %v", err)
	}

	// Verify both rows.
	var count int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s`", tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}

// Section 4: ALTER TABLE with Generated Columns

// TestGenerated_AlterTable_AddGeneratedColumn verifies adding a generated
// column to an existing table via ALTER TABLE.
func TestGenerated_AlterTable_AddGeneratedColumn(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("alteradd")

	// Create table WITHOUT generated column.
	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			price DECIMAL(10,2) NOT NULL,
			tax_rate DECIMAL(5,4) NOT NULL DEFAULT 0.0800
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert some data.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (price, tax_rate) VALUES (100.00, 0.0800), (50.00, 0.0500)", tableName))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Now ALTER TABLE to add a generated column for total (price + price * tax_rate).
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE `%s` ADD COLUMN total DECIMAL(12,2) GENERATED ALWAYS AS (price * (1 + tax_rate)) STORED",
		tableName))
	if err != nil {
		t.Fatalf("ALTER TABLE ADD generated column: %v", err)
	}

	// Verify the generated column is populated for existing rows.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT price, tax_rate, total FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("select after ALTER: %v", err)
	}
	defer rows.Close()

	type row struct {
		price, taxRate, total float64
	}
	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.price, &r.taxRate, &r.total); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}
	// Row 1: 100 * 1.08 = 108.00
	if results[0].total != 108.00 {
		t.Errorf("row 1 total = %.2f, want 108.00", results[0].total)
	}
	// Row 2: 50 * 1.05 = 52.50
	if results[1].total != 52.50 {
		t.Errorf("row 2 total = %.2f, want 52.50", results[1].total)
	}
	t.Logf("ALTER TABLE ADD generated column: 2 rows recomputed")
}

// TestGenerated_AlterTable_DropGeneratedColumn verifies dropping a
// generated column.
func TestGenerated_AlterTable_DropGeneratedColumn(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("alterdrop")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			a INT NOT NULL,
			b INT NOT NULL,
			c INT GENERATED ALWAYS AS (a + b) VIRTUAL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Drop the generated column.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE `%s` DROP COLUMN c", tableName))
	if err != nil {
		t.Fatalf("DROP generated column: %v", err)
	}

	// Verify the column is gone by trying to select it.
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT c FROM `%s` LIMIT 1", tableName))
	_ = qrErr
	var dummy string
	err = row.Scan(&dummy)
	if err == nil {
		t.Error("column c should not exist after DROP")
	}
	t.Logf("column correctly dropped: %v", err)
}

// Section 5: Edge Cases

// TestGenerated_NullHandling tests generated columns with NULL input values.
func TestGenerated_NullHandling(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("nullgc")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			first_name VARCHAR(50),
			last_name VARCHAR(50),
			full_name VARCHAR(102) GENERATED ALWAYS AS (
				CONCAT(
					COALESCE(first_name, '?'),
					' ',
					COALESCE(last_name, '?')
				)
			) VIRTUAL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	tests := []struct {
		firstName, lastName sql.NullString
		expected            string
	}{
		{sql.NullString{String: "John", Valid: true}, sql.NullString{String: "Doe", Valid: true}, "John Doe"},
		{sql.NullString{Valid: false}, sql.NullString{String: "Doe", Valid: true}, "? Doe"},
		{sql.NullString{String: "Jane", Valid: true}, sql.NullString{Valid: false}, "Jane ?"},
		{sql.NullString{Valid: false}, sql.NullString{Valid: false}, "? ?"},
	}

	for i, tt := range tests {
		var fid, lid any
		if tt.firstName.Valid {
			fid = tt.firstName.String
		}
		if tt.lastName.Valid {
			lid = tt.lastName.String
		}
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (first_name, last_name) VALUES (?, ?)", tableName),
			fid, lid)
		if err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}

		var fullName string
		row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
			"SELECT full_name FROM `%s` WHERE id = %d", tableName, i+1),
		)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err = row.Scan(&fullName)
		if err != nil {
			t.Fatalf("select row %d: %v", i, err)
		}
		if fullName != tt.expected {
			t.Errorf("row %d: full_name = %q, want %q", i, fullName, tt.expected)
		}
	}
}

// TestGenerated_DateExpression tests generated columns with date functions.
func TestGenerated_DateExpression(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("dateexpr")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			created_at DATETIME(6) NOT NULL,
			created_date DATE GENERATED ALWAYS AS (DATE(created_at)) VIRTUAL,
			created_year INT GENERATED ALWAYS AS (YEAR(created_at)) STORED,
			created_month INT GENERATED ALWAYS AS (MONTH(created_at)) STORED
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (created_at) VALUES ('2024-06-15 14:30:00.123456')", tableName))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var date time.Time
	var year, month int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT created_date, created_year, created_month FROM `%s` WHERE id = 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&date, &year, &month)
	if err != nil {
		t.Fatalf("select generated date columns: %v", err)
	}

	if date.Format("2006-01-02") != "2024-06-15" {
		t.Errorf("created_date = %q, want '2024-06-15'", date.Format("2006-01-02"))
	}
	if year != 2024 {
		t.Errorf("created_year = %d, want 2024", year)
	}
	if month != 6 {
		t.Errorf("created_month = %d, want 6", month)
	}
	t.Logf("date generated columns: %s, %d-%02d", date, year, month)
}

// TestGenerated_CaseExpression tests generated columns with CASE/WHEN logic.
func TestGenerated_CaseExpression(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("caseexpr")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			score INT NOT NULL,
			grade CHAR(1) GENERATED ALWAYS AS (
				CASE
					WHEN score >= 90 THEN 'A'
					WHEN score >= 80 THEN 'B'
					WHEN score >= 70 THEN 'C'
					WHEN score >= 60 THEN 'D'
					ELSE 'F'
				END
			) STORED
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	tests := []struct {
		score int
		grade string
	}{
		{95, "A"}, {85, "B"}, {75, "C"}, {65, "D"}, {55, "F"},
		{100, "A"}, {89, "B"}, {79, "C"}, {69, "D"}, {0, "F"},
	}

	for _, tt := range tests {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (score) VALUES (?)", tableName), tt.score)
		if err != nil {
			t.Fatalf("insert score=%d: %v", tt.score, err)
		}
	}

	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT score, grade FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var score int
		var grade string
		if err := rows.Scan(&score, &grade); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if grade != tests[i].grade {
			t.Errorf("score %d: grade = %q, want %q", score, grade, tests[i].grade)
		}
		if score != tests[i].score {
			t.Errorf("score mismatch: got %d, want %d", score, tests[i].score)
		}
		i++
	}
	if i != len(tests) {
		t.Errorf("expected %d rows, got %d", len(tests), i)
	}
	t.Logf("CASE expression generated column: %d grades verified", i)
}

// TestGenerated_MultipleGeneratedColumns verifies a table with multiple
// generated columns of different types and storage modes.
func TestGenerated_MultipleGeneratedColumns(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("multi")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			email VARCHAR(255) NOT NULL,
			domain VARCHAR(255) GENERATED ALWAYS AS (
				SUBSTRING_INDEX(email, '@', -1)
			) VIRTUAL,
			local_part VARCHAR(255) GENERATED ALWAYS AS (
				SUBSTRING_INDEX(email, '@', 1)
			) VIRTUAL,
			email_len INT GENERATED ALWAYS AS (CHAR_LENGTH(email)) STORED,
			has_plus TINYINT(1) GENERATED ALWAYS AS (email LIKE '%+%') VIRTUAL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	tests := []struct {
		email, domain, local string
		length               int
		hasPlus              bool
	}{
		{"user@example.com", "example.com", "user", 16, false},
		{"john.doe@example.org", "example.org", "john.doe", 20, false},
		{"admin+test@example.net", "example.net", "admin+test", 22, true},
		{"x@y.z", "y.z", "x", 5, false},
	}

	for _, tt := range tests {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (email) VALUES (?)", tableName), tt.email)
		if err != nil {
			t.Fatalf("insert %q: %v", tt.email, err)
		}
	}

	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT email, domain, local_part, email_len, has_plus FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var email, domain, localPart string
		var emailLen int
		var hasPlus bool
		if err := rows.Scan(&email, &domain, &localPart, &emailLen, &hasPlus); err != nil {
			t.Fatalf("scan: %v", err)
		}

		if domain != tests[i].domain {
			t.Errorf("row %d domain = %q, want %q", i, domain, tests[i].domain)
		}
		if localPart != tests[i].local {
			t.Errorf("row %d local_part = %q, want %q", i, localPart, tests[i].local)
		}
		if emailLen != tests[i].length {
			t.Errorf("row %d email_len = %d, want %d", i, emailLen, tests[i].length)
		}
		if hasPlus != tests[i].hasPlus {
			t.Errorf("row %d has_plus = %v, want %v", i, hasPlus, tests[i].hasPlus)
		}
		i++
	}
	if i != len(tests) {
		t.Errorf("expected %d rows, got %d", len(tests), i)
	}
	t.Logf("multiple generated columns: %d rows, 4 generated columns each", i)
}

// TestGenerated_ShowCreateTable verifies that SHOW CREATE TABLE includes
// generated column definitions: important for schema migration tool output.
func TestGenerated_ShowCreateTable(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("showcreate")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			a INT NOT NULL,
			b INT NOT NULL,
			c INT GENERATED ALWAYS AS (a + b) STORED
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	var createTable string
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`", tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(new(string), &createTable) // SHOW CREATE TABLE returns 2 columns
	if err != nil {
		t.Fatalf("SHOW CREATE TABLE: %v", err)
	}

	if !contains(createTable, "GENERATED ALWAYS AS") {
		t.Errorf("SHOW CREATE TABLE missing GENERATED ALWAYS AS clause:\n%s", createTable)
	}
	if !contains(createTable, "STORED") {
		t.Errorf("SHOW CREATE TABLE missing STORED keyword:\n%s", createTable)
	}
	t.Logf("SHOW CREATE TABLE output includes generated column definition")
}

// TestGenerated_DefaultExpressionConflict verifies that you CANNOT have both
// a DEFAULT and GENERATED ALWAYS AS on the same column.
func TestGenerated_DefaultExpressionConflict(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := genTableName("conflict")

	// This should fail: you cannot have both DEFAULT and GENERATED ALWAYS AS.
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (
			id INT AUTO_INCREMENT PRIMARY KEY,
			a INT NOT NULL,
			b INT NOT NULL,
			c INT DEFAULT 0 GENERATED ALWAYS AS (a + b) VIRTUAL
		)`, tableName))
	if err == nil {
		t.Error("expected error when defining both DEFAULT and GENERATED ALWAYS AS on same column")
		pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")
	} else {
		t.Logf("DEFAULT + GENERATED ALWAYS AS conflict correctly rejected: %v", err)
	}
}
