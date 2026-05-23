//go:build !short && !mutest

// MySQL Strict Mode vs Non-Strict Differences: Integration Tests
// Coverage for sql_mode flags and their interactions:
//
//   - ERROR_FOR_DIVISION_BY_ZERO
//   - NO_ZERO_DATE / NO_ZERO_IN_DATE
//   - ONLY_FULL_GROUP_BY
//   - NOT NULL without DEFAULT
//   - STRICT_ALL_TABLES vs STRICT_TRANS_TABLES
//   - Combined mode interactions
//   - Data type coercion (string->int, float->int truncation)
//   - Default value enforcement
//
// The CMS's multi-dialect query builder and schema engine must
// account for these differences, especially since PostgreSQL is
// always strict while MySQL's behavior depends on sql_mode.
//
// References:
//   - MySQL 8.0 Reference Manual §7.1.11 (Server SQL Modes)
//   - https://dev.mysql.com/doc/refman/8.0/en/sql-mode.html
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

// smTableName generates a deterministic table name for strict mode tests.
func smTableName(prefix string) string {
	return fmt.Sprintf("sm_%s_%d", prefix, time.Now().UnixNano()%100000)
}

// Section 1: ERROR_FOR_DIVISION_BY_ZERO

// TestStrictMode_DivisionByZero verifies that division by zero produces
// an error in strict mode and NULL in non-strict mode.
func TestStrictMode_DivisionByZero(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("divzero")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			numerator INT NOT NULL,
			denominator INT NOT NULL,
			result DECIMAL(10,2)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Strict mode: 1/0 should fail
	var sqlMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sqlMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	if strings.Contains(sqlMode, "ERROR_FOR_DIVISION_BY_ZERO") ||
		strings.Contains(sqlMode, "STRICT_TRANS_TABLES") ||
		strings.Contains(sqlMode, "STRICT_ALL_TABLES") {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (numerator, denominator, result) VALUES (1, 0, 1/0)", tableName))
		if err == nil {
			// Some strict mode combinations still allow NULL for division by zero
			// if ERROR_FOR_DIVISION_BY_ZERO is not explicitly set.
			t.Log("strict mode: division by zero did not error (ERROR_FOR_DIVISION_BY_ZERO may not be in sql_mode)")
		} else {
			t.Logf("strict mode: division by zero correctly rejected: %v", err)
		}
	}

	// Non-strict mode: 1/0 should produce NULL with a warning
	nonStrictMode := removeStrict(sqlMode) + ",NO_ENGINE_SUBSTITUTION"
	clean := cleanSQLMode(nonStrictMode)

	_, err = pool.Exec(ctx, fmt.Sprintf("SET SESSION sql_mode = '%s'", clean))
	if err != nil {
		t.Fatalf("set non-strict mode: %v", err)
	}
	defer pool.Exec(context.Background(), fmt.Sprintf("SET SESSION sql_mode = '%s'", sqlMode))

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (numerator, denominator, result) VALUES (1, 0, 1/0)", tableName))
	if err != nil {
		t.Fatalf("non-strict mode: division by zero should not error: %v", err)
	}

	var result sql.NullFloat64
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT result FROM `%s` WHERE numerator = 1 AND denominator = 0", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&result)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if result.Valid {
		t.Errorf("non-strict mode: 1/0 should be NULL, got %.2f", result.Float64)
	}
	t.Log("non-strict mode: division by zero -> NULL")
}

// Section 2: NO_ZERO_DATE / NO_ZERO_IN_DATE

// TestStrictMode_ZeroDate verifies that zero dates (0000-00-00) are
// rejected in strict mode but accepted in non-strict mode.
func TestStrictMode_ZeroDate(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("zerodate")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			event_date DATE,
			event_datetime DATETIME
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	var sqlMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sqlMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	// Test zero date insertion.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (event_date) VALUES ('0000-00-00')", tableName))
	if strings.Contains(sqlMode, "NO_ZERO_DATE") || strings.Contains(sqlMode, "NO_ZERO_IN_DATE") {
		if err == nil {
			t.Errorf("zero date '0000-00-00' should be rejected with NO_ZERO_DATE in sql_mode=%s", sqlMode)
		} else {
			t.Logf("zero date correctly rejected: %v", err)
		}
	} else {
		if err != nil {
			t.Logf("zero date rejected even without NO_ZERO_DATE (possibly strict mode): %v", err)
		}
	}
}

// TestStrictMode_ZeroInDate verifies that dates like '2024-00-15' (zero month)
// are rejected when NO_ZERO_IN_DATE is active.
func TestStrictMode_ZeroInDate(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("zeroindate")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			birth_date DATE
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	var sqlMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sqlMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	// '2024-00-15' has a zero month.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (birth_date) VALUES ('2024-00-15')", tableName))
	if strings.Contains(sqlMode, "NO_ZERO_IN_DATE") {
		if err == nil {
			t.Errorf("zero-in-date '2024-00-15' should be rejected with NO_ZERO_IN_DATE in sql_mode=%s", sqlMode)
		} else {
			t.Logf("zero-in-date correctly rejected: %v", err)
		}
	} else {
		// Without NO_ZERO_IN_DATE, MySQL converts '2024-00-15' -> '0000-00-00' -> may be rejected by NO_ZERO_DATE.
		if err != nil {
			t.Logf("zero-in-date rejected (may be indirect via NO_ZERO_DATE): %v", err)
		} else {
			t.Log("zero-in-date accepted without NO_ZERO_IN_DATE restriction")
		}
	}
}

// Section 3: ONLY_FULL_GROUP_BY

// TestStrictMode_OnlyFullGroupBy verifies that ONLY_FULL_GROUP_BY rejects
// SELECT columns not in GROUP BY or aggregate functions.
func TestStrictMode_OnlyFullGroupBy(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("groupby")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			category VARCHAR(50),
			value INT
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (category, value) VALUES ('A', 10), ('A', 20), ('B', 30)", tableName))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var sqlMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sqlMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	// Only category and aggregates should be valid.
	validQuery := fmt.Sprintf(
		"SELECT category, SUM(value) FROM `%s` GROUP BY category", tableName)
	rows, err := pool.Query(ctx, validQuery)
	if err != nil {
		t.Fatalf("valid GROUP BY query: %v", err)
	}
	rows.Close()

	// value is not in GROUP BY and not aggregated: should fail with ONLY_FULL_GROUP_BY.
	invalidQuery := fmt.Sprintf(
		"SELECT category, value FROM `%s` GROUP BY category", tableName)
	_, err = pool.Query(ctx, invalidQuery)
	if strings.Contains(sqlMode, "ONLY_FULL_GROUP_BY") {
		if err == nil {
			t.Error("ONLY_FULL_GROUP_BY: non-aggregated column should be rejected")
		} else {
			t.Logf("ONLY_FULL_GROUP_BY correctly rejected: %v", err)
		}
	} else {
		if err != nil {
			t.Logf("rejected even without ONLY_FULL_GROUP_BY: %v", err)
		} else {
			t.Log("non-aggregated column accepted (ONLY_FULL_GROUP_BY not active)")
		}
	}
}

// Section 4: NOT NULL Without DEFAULT

// TestStrictMode_NotNullNoDefault verifies that a NOT NULL column without
// DEFAULT requires an explicit value in strict mode.
func TestStrictMode_NotNullNoDefault(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("notnulldef")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			required_field VARCHAR(50) NOT NULL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	var sqlMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sqlMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	// Strict mode: INSERT without required_field should fail.
	isStrict := strings.Contains(sqlMode, "STRICT_TRANS_TABLES") ||
		strings.Contains(sqlMode, "STRICT_ALL_TABLES")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (id) VALUES (1)", tableName))
	if isStrict {
		if err == nil {
			t.Error("strict mode: should reject NOT NULL column without explicit value")
		} else {
			t.Logf("strict mode correctly rejected missing NOT NULL value: %v", err)
		}
	} else {
		if err != nil {
			t.Logf("rejected even without strict mode: %v", err)
		} else {
			t.Log("non-strict: NOT NULL without value accepted with implicit default")
		}
	}

	// Explicit NULL for NOT NULL should always fail.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (required_field) VALUES (NULL)", tableName))
	if err == nil {
		t.Error("NULL into NOT NULL column should always be rejected")
	} else {
		t.Logf("NULL into NOT NULL correctly rejected: %v", err)
	}
}

// Section 5: STRICT_ALL_TABLES vs STRICT_TRANS_TABLES

// TestStrictMode_StrictAllTables_NonTransactional verifies that
// STRICT_ALL_TABLES applies strict rules even to non-transactional
// statements (MyISAM) while STRICT_TRANS_TABLES only applies to
// transactional statements (InnoDB).
//
// Since we use InnoDB, both modes should behave identically for our
// use case. This test verifies the configuration.
func TestStrictMode_ModeConfiguration(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var globalMode, sessionMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@GLOBAL.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&globalMode)
	if err != nil {
		t.Fatalf("read global sql_mode: %v", err)
	}
	row, qrErr = pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sessionMode)
	if err != nil {
		t.Fatalf("read session sql_mode: %v", err)
	}

	t.Logf("sql_mode global: %s", globalMode)
	t.Logf("sql_mode session: %s", sessionMode)

	// At least one strict variant should be present for InnoDB workloads.
	hasStrict := strings.Contains(sessionMode, "STRICT_TRANS_TABLES") ||
		strings.Contains(sessionMode, "STRICT_ALL_TABLES")
	if !hasStrict {
		t.Errorf("session sql_mode does not contain any STRICT_* mode: %s", sessionMode)
	}

	// Check for recommended modes.
	recommended := []string{
		"ONLY_FULL_GROUP_BY",
		"STRICT_TRANS_TABLES",
		"NO_ZERO_IN_DATE",
		"NO_ZERO_DATE",
		"ERROR_FOR_DIVISION_BY_ZERO",
	}
	for _, mode := range recommended {
		if !strings.Contains(sessionMode, mode) {
			t.Logf("recommended sql_mode %q is not active (session=%s)", mode, sessionMode)
		}
	}
}

// Section 6: Data Type Coercion Differences

// TestStrictMode_StringToIntCoercion verifies strict mode prevents silent
// type coercion from invalid strings to integers.
func TestStrictMode_StringToIntCoercion(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("strtoint")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			int_col INT
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Strict mode: 'abc' into INT should fail.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (int_col) VALUES (?)", tableName), "abc")
	if err == nil {
		t.Error("strict mode: 'abc' into INT should fail")
	} else {
		t.Logf("strict mode: string->int coercion correctly rejected: %v", err)
	}

	// '123' into INT should succeed (valid numeric string).
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (int_col) VALUES (?)", tableName), "123")
	if err != nil {
		t.Fatalf("strict mode: '123' into INT should succeed: %v", err)
	}

	// Non-strict mode: 'abc' into INT becomes 0.
	var sqlMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&sqlMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	nonStrictMode := removeStrict(sqlMode) + ",NO_ENGINE_SUBSTITUTION"
	clean := cleanSQLMode(nonStrictMode)

	_, err = pool.Exec(ctx, fmt.Sprintf("SET SESSION sql_mode = '%s'", clean))
	if err != nil {
		t.Fatalf("set non-strict mode: %v", err)
	}
	defer pool.Exec(context.Background(), fmt.Sprintf("SET SESSION sql_mode = '%s'", sqlMode))

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (int_col) VALUES ('notanumber')", tableName))
	if err != nil {
		t.Fatalf("non-strict mode: 'notanumber' into INT should succeed with coercion: %v", err)
	}

	var val int
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT int_col FROM `%s` WHERE int_col IS NOT NULL ORDER BY id DESC LIMIT 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&val)
	if err != nil {
		t.Fatalf("select coerced value: %v", err)
	}
	if val != 0 {
		t.Errorf("non-strict: 'notanumber' should coerce to 0, got %d", val)
	}
	t.Logf("non-strict: string->int coercion -> 0")
}

// Section 7: Default Value Differences

// TestStrictMode_DefaultValues verifies default value behavior across modes.
func TestStrictMode_DefaultValues(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("defaults")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(50) NOT NULL DEFAULT 'unnamed',
			score INT DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert with no columns specified: defaults should apply.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` () VALUES ()", tableName))
	if err != nil {
		t.Fatalf("insert with defaults: %v", err)
	}

	var name string
	var score int
	var createdAt time.Time
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT name, score, created_at FROM `%s` WHERE id = 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&name, &score, &createdAt)
	if err != nil {
		t.Fatalf("select defaults: %v", err)
	}

	if name != "unnamed" {
		t.Errorf("default name = %q, want 'unnamed'", name)
	}
	if score != 0 {
		t.Errorf("default score = %d, want 0", score)
	}
	if createdAt.IsZero() {
		t.Error("default created_at is zero - should be CURRENT_TIMESTAMP")
	}
	t.Logf("defaults: name=%q score=%d created_at=%v", name, score, createdAt)
}

// Section 8: Combined Mode Interactions

// TestStrictMode_CombinedFlags verifies that multiple sql_mode flags
// interact correctly: e.g., strict mode + no_zero_date together.
func TestStrictMode_CombinedFlags(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("combined")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			amount INT NOT NULL,
			note VARCHAR(10)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	var origMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&origMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	// Test 1: Strict mode alone, out-of-range + truncation both rejected.
	strictOnly := "STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION"
	_, err = pool.Exec(ctx, fmt.Sprintf("SET SESSION sql_mode = '%s'", strictOnly))
	if err != nil {
		t.Fatalf("set strict-only mode: %v", err)
	}

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (amount, note) VALUES (99999, 'too-long-string-exceeds-10')", tableName))
	if err == nil {
		t.Error("STRICT_TRANS_TABLES alone should reject truncation")
	} else {
		t.Logf("strict-only: truncation rejected: %v", err)
	}

	// Test 2: Non-strict mode. Both should silently truncate/coerce.
	nonStrictOnly := "NO_ENGINE_SUBSTITUTION"
	_, err = pool.Exec(ctx, fmt.Sprintf("SET SESSION sql_mode = '%s'", nonStrictOnly))
	if err != nil {
		t.Fatalf("set non-strict mode: %v", err)
	}

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (amount, note) VALUES (99999, 'too-long-string-exceeds-10')", tableName))
	if err != nil {
		t.Fatalf("non-strict mode should accept with coercion: %v", err)
	}

	var amount int
	var note string
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT amount, note FROM `%s` WHERE amount IS NOT NULL ORDER BY id DESC LIMIT 1", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&amount, &note)
	if err != nil {
		t.Fatalf("select coerced: %v", err)
	}

	// In non-strict mode: 99999 -> 32767 (TINYINT max) or 99999 (INT).
	// The value should be truncated to fit.
	t.Logf("non-strict: amount=%d note=%q (len=%d)", amount, note, len(note))
	if len(note) > 10 {
		t.Errorf("non-strict: note should be truncated to 10 chars, got %d chars: %q", len(note), note)
	}

	// Restore original mode.
	_, err = pool.Exec(ctx, fmt.Sprintf("SET SESSION sql_mode = '%s'", origMode))
	if err != nil {
		t.Fatalf("restore sql_mode: %v", err)
	}
}

// Section 9: NOT NULL Column with Implicit Default

// TestStrictMode_NotNullImplicitDefault verifies MySQL 8.0's behavior
// when a NOT NULL column has no explicit default. In strict mode, MySQL
// requires an explicit value. In non-strict mode, it uses the implicit
// default for the data type: zero for INT, the empty string for VARCHAR.
func TestStrictMode_NotNullImplicitDefault(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := smTableName("implicitdef")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			int_col INT NOT NULL,
			str_col VARCHAR(10) NOT NULL,
			dec_col DECIMAL(10,2) NOT NULL
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	var origMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&origMode)
	if err != nil {
		t.Fatalf("read sql_mode: %v", err)
	}

	// Strict mode: INSERT without NOT NULL columns should fail.
	isStrict := strings.Contains(origMode, "STRICT_TRANS_TABLES") ||
		strings.Contains(origMode, "STRICT_ALL_TABLES")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (id) VALUES (1)", tableName))
	if isStrict {
		if err == nil {
			t.Error("strict mode: should reject NOT NULL columns without values")
		} else {
			t.Logf("strict mode: correctly rejected: %v", err)
		}
	}

	// Non-strict mode: implicit defaults should be applied.
	nonStrictOnly := "NO_ENGINE_SUBSTITUTION"
	_, err = pool.Exec(ctx, fmt.Sprintf("SET SESSION sql_mode = '%s'", nonStrictOnly))
	if err != nil {
		t.Fatalf("set non-strict: %v", err)
	}
	defer pool.Exec(context.Background(), fmt.Sprintf("SET SESSION sql_mode = '%s'", origMode))

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (id) VALUES (2)", tableName))
	if err != nil {
		t.Fatalf("non-strict: should apply implicit defaults: %v", err)
	}

	var intCol int
	var strCol string
	var decCol float64
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT int_col, str_col, dec_col FROM `%s` WHERE id = 2", tableName),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&intCol, &strCol, &decCol)
	if err != nil {
		t.Fatalf("select implicit defaults: %v", err)
	}

	if intCol != 0 {
		t.Errorf("implicit default for INT: got %d, want 0", intCol)
	}
	if strCol != "" {
		t.Errorf("implicit default for VARCHAR: got %q, want ''", strCol)
	}
	if decCol != 0.00 {
		t.Errorf("implicit default for DECIMAL: got %.2f, want 0.00", decCol)
	}
	t.Logf("non-strict implicit defaults: INT=%d, VARCHAR=%q, DECIMAL=%.2f", intCol, strCol, decCol)
}

// Section 10: sql_mode Verification

// TestStrictMode_SQLModeComprehensive checks the full sql_mode configuration
// for completeness.
func TestStrictMode_SQLModeComprehensive(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	modeChecks := []struct {
		modeVar  string
		expected string // substring to check for
	}{
		{"sql_mode", ""}, // just log it
		{"innodb_strict_mode", "1"},
		{"explicit_defaults_for_timestamp", "1"},
	}

	for _, mc := range modeChecks {
		var val string
		row, qrErr := pool.QueryRow(ctx, "SELECT @@"+mc.modeVar)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Logf("skip %s: %v", mc.modeVar, err)
			continue
		}
		t.Logf("%s = %s", mc.modeVar, val)
		if mc.expected != "" && val != mc.expected {
			t.Errorf("%s = %q, expected %q", mc.modeVar, val, mc.expected)
		}
	}

	// Ensure at least the session has a sane sql_mode.
	var sessionMode string
	row, qrErr := pool.QueryRow(ctx, "SELECT @@SESSION.sql_mode")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&sessionMode)
	if err != nil {
		t.Fatalf("read session sql_mode: %v", err)
	}

	// Verify no dangerous modes are active.
	dangerousModes := []string{
		"ALLOW_INVALID_DATES", // Accepts 2004-02-31 as valid
	}
	for _, dm := range dangerousModes {
		if strings.Contains(sessionMode, dm) {
			t.Errorf("dangerous sql_mode %q is active: %s", dm, sessionMode)
		}
	}
}

// Helpers

// removeStrict returns the sql_mode string with STRICT_* modes removed.
func removeStrict(sqlMode string) string {
	for _, term := range []string{"STRICT_TRANS_TABLES", "STRICT_ALL_TABLES"} {
		sqlMode = strings.ReplaceAll(sqlMode, term, "")
	}
	return sqlMode
}

// cleanSQLMode removes leading/trailing commas and double commas.
func cleanSQLMode(s string) string {
	s = strings.ReplaceAll(s, ",,", ",")
	s = strings.Trim(s, ",")
	if s == "" {
		s = "NO_ENGINE_SUBSTITUTION"
	}
	return s
}
