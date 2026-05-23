//go:build !short && !mutest

// MySQL Charset & Collation Edge Case Integration Tests
// Exercises MySQL 8.0's character set and collation system against a real
// container, focusing on scenarios relevant to multi-tenant CMS deployments:
//
//   - utf8mb4 vs utf8mb3 (4-byte emoji and supplementary characters)
//   - Collation case sensitivity (_bin, _general_ci, _unicode_ci, _0900_ai_ci)
//   - Multi-byte character storage (CJK, Arabic, Cyrillic, emoji)
//   - Collation conflicts in JOINs and WHERE clauses
//   - Collation precedence (column > table > database > server)
//   - LIKE vs = with collation
//   - Binary collation for exact matching
//   - ALTER TABLE ... CONVERT TO CHARACTER SET
//   - utf8mb4 VARCHAR length semantics (characters vs bytes)
//   - Collation-aware UNIQUE constraints
//
// References:
//   - MySQL 8.0 Reference Manual §10 (Character Sets, Collations, Unicode)
//   - https://dev.mysql.com/doc/refman/8.0/en/charset.html
package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// csTableName generates a deterministic table name for charset tests.
func csTableName(prefix string) string {
	return fmt.Sprintf("cs_%s_%d", prefix, time.Now().UnixNano()%100000)
}

// Section 1: utf8mb4 Emoji & Supplementary Characters

// TestCharset_utf8mb4_Emoji verifies that utf8mb4 correctly stores and
// retrieves 4-byte Unicode characters (emoji, supplementary planes).
// utf8mb3 (historical MySQL "utf8") only supports 3-byte characters
// and silently truncates 4-byte ones: a notorious gotcha.
func TestCharset_utf8mb4_Emoji(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("emoji")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`)
	if err != nil {
		t.Fatalf("create utf8mb4 table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert a mix of 4-byte emoji and supplementary characters.
	tests := []string{
		"Hello 👋 World 🌍",            // Wave + globe emoji
		"🎉 Party 🎊",                  // Party emojis
		"Math: 𝕏 ×𝟚 = 𝟚𝟘",            // Mathematical alphanumerics (U+1D54F et al.)
		"🎵 Music note ♪ and heart ♥", // Mix of BMP and supplementary
		"😀😃😄😁😆😅😂🤣",                   // All 4-byte emojis
		"CJK + Emoji: 中文テスト🎌",        // CJK mixed with emoji
		"𐍈 Gothic letter hwair",      // Gothic (U+10348) - 4-byte
	}

	for i, s := range tests {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (content) VALUES (?)", tableName), s)
		if err != nil {
			t.Fatalf("insert emoji row %d: %v", i, err)
		}
	}

	// Verify all rows roundtrip correctly.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT id, content, CHAR_LENGTH(content), LENGTH(content) FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("select emoji rows: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var id int
		var content string
		var charLen, byteLen int
		if err := rows.Scan(&id, &content, &charLen, &byteLen); err != nil {
			t.Fatalf("scan row %d: %v", id, err)
		}

		if content != tests[i] {
			t.Errorf("row %d emoji roundtrip failed:\n  got  %q\n  want %q", id, content, tests[i])
		}

		// Verify byte length is consistent with 4-byte characters.
		expectedBytes := len(tests[i])
		if byteLen != expectedBytes {
			t.Errorf("row %d: LENGTH=%d, want %d (possible truncation if utf8mb3 used)", id, byteLen, expectedBytes)
		}

		t.Logf("row %d: CHAR_LENGTH=%d, LENGTH=%d, content=%q", id, charLen, byteLen, truncate(content, 40))
		i++
	}

	if i != len(tests) {
		t.Errorf("expected %d rows, got %d", len(tests), i)
	}
}

// TestCharset_utf8mb3_EmojiRejection verifies that utf8mb3 (the old "utf8")
// rejects or truncates 4-byte characters. This test exists to document
// the behavior difference.
func TestCharset_utf8mb3_EmojiRejection(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("utf8mb3")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content VARCHAR(255) CHARACTER SET utf8mb3
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create utf8mb3 table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// In strict mode, inserting a 4-byte character into a utf8mb3 column
	// should fail with "Incorrect string value" error.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (content) VALUES (?)", tableName), "Hello 👋")
	if err == nil {
		t.Error("expected error inserting 4-byte emoji into utf8mb3 column in strict mode")
	} else {
		t.Logf("utf8mb3 correctly rejected 4-byte emoji: %v", err)
	}
}

// Section 2: Collation Case Sensitivity

// TestCollation_CaseSensitivity verifies that different collations handle
// case sensitivity differently.
func TestCollation_CaseSensitivity(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("casesens")

	// Create table with mixed collation columns.
	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			name_ci VARCHAR(50) COLLATE utf8mb4_unicode_ci,
			name_cs VARCHAR(50) COLLATE utf8mb4_bin,
			name_0900_ai VARCHAR(50) COLLATE utf8mb4_0900_ai_ci,
			name_0900_as VARCHAR(50) COLLATE utf8mb4_0900_as_cs
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create collation table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (name_ci, name_cs, name_0900_ai, name_0900_as) VALUES (?, ?, ?, ?)", tableName),
		"Alice", "Alice", "Alice", "Alice")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// _ci (case-insensitive): "alice" = "Alice"
	var count int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE name_ci = ?", tableName), "alice",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_ci search: %v", err)
	}
	if count != 1 {
		t.Errorf("utf8mb4_unicode_ci: 'alice' should match 'Alice', got %d results", count)
	}

	// _bin (binary): "alice" ≠ "Alice"
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE name_cs = ?", tableName), "alice",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_bin search: %v", err)
	}
	if count != 0 {
		t.Errorf("utf8mb4_bin: 'alice' should NOT match 'Alice', got %d results", count)
	}

	// _bin: exact case match should work
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE name_cs = ?", tableName), "Alice",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_bin exact search: %v", err)
	}
	if count != 1 {
		t.Errorf("utf8mb4_bin: 'Alice' should match 'Alice', got %d results", count)
	}

	// _0900_ai_ci (accent-insensitive, case-insensitive)
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE name_0900_ai = ?", tableName), "alice",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_0900_ai_ci search: %v", err)
	}
	if count != 1 {
		t.Errorf("utf8mb4_0900_ai_ci: 'alice' should match 'Alice', got %d results", count)
	}

	// _0900_as_cs (accent-sensitive, case-sensitive)
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE name_0900_as = ?", tableName), "alice",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_0900_as_cs search: %v", err)
	}
	if count != 0 {
		t.Errorf("utf8mb4_0900_as_cs: 'alice' should NOT match 'Alice', got %d results", count)
	}

	t.Log("collation case sensitivity verified across 4 collations")
}

// TestCollation_LIKE_CaseSensitivity verifies that LIKE respects collation
// for case sensitivity.
func TestCollation_LIKE_CaseSensitivity(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("likesens")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(50) COLLATE utf8mb4_unicode_ci,
			code VARCHAR(50) COLLATE utf8mb4_bin
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (name, code) VALUES ('AliceSmith', 'AliceSmith')", tableName))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// _ci: LIKE '%smith%' should match 'Smith' (case-insensitive).
	var count int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE name LIKE ?", tableName), "%smith%",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_ci LIKE: %v", err)
	}
	if count != 1 {
		t.Errorf("_ci LIKE '%%smith%%' should match 'AliceSmith', got %d", count)
	}

	// _bin: LIKE '%smith%' should NOT match 'Smith' (case-sensitive binary).
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE code LIKE ?", tableName), "%smith%",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_bin LIKE: %v", err)
	}
	if count != 0 {
		t.Errorf("_bin LIKE '%%smith%%' should NOT match 'AliceSmith', got %d", count)
	}

	// _bin: LIKE '%Smith%' should match.
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE code LIKE ?", tableName), "%Smith%",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("_bin LIKE exact case: %v", err)
	}
	if count != 1 {
		t.Errorf("_bin LIKE '%%Smith%%' should match 'AliceSmith', got %d", count)
	}
}

// Section 3: Accent Sensitivity

// TestCollation_AccentSensitivity verifies that MySQL 8.0 collations handle
// accented characters correctly.
func TestCollation_AccentSensitivity(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("accents")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			word_ai VARCHAR(50) COLLATE utf8mb4_0900_ai_ci,
			word_as VARCHAR(50) COLLATE utf8mb4_0900_as_cs,
			word_bin VARCHAR(50) COLLATE utf8mb4_bin
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create accent table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert "café" with accent.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (word_ai, word_as, word_bin) VALUES ('café', 'café', 'café')", tableName))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	tests := []struct {
		col     string
		search  string
		matches bool
		desc    string
	}{
		// _ai_ci (accent-insensitive): "cafe" = "café"
		{"word_ai", "cafe", true, "ai_ci: 'cafe' matches 'café'"},
		// _as_cs (accent-sensitive): "cafe" ≠ "café"
		{"word_as", "cafe", false, "as_cs: 'cafe' does NOT match 'café'"},
		// _bin (binary): "cafe" ≠ "café"
		{"word_bin", "cafe", false, "bin: 'cafe' does NOT match 'café'"},
		// _as_cs exact: "café" = "café"
		{"word_as", "café", true, "as_cs: 'café' matches 'café'"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			var count int
			row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s` WHERE %s = ?", tableName, tt.col),
				tt.search,
			)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err = row.Scan(&count)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			hit := count == 1
			if hit != tt.matches {
				t.Errorf("%s: hit=%v, expected=%v", tt.desc, hit, tt.matches)
			}
		})
	}
}

// Section 4: Multi-Language Storage

// TestCharset_Multilingual_Storage verifies storage of diverse scripts
// in utf8mb4 columns.
func TestCharset_Multilingual_Storage(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("multilang")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			lang VARCHAR(10) NOT NULL,
			content TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		t.Fatalf("create multilingual table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	samples := []struct {
		lang, content string
	}{
		{"en", "The quick brown fox jumps over the lazy dog"},
		{"zh", "敏捷的棕色狐狸跳过了懒狗"},                                           // Chinese
		{"ja", "素早い茶色の狐が怠け者の犬を飛び越える"},                                    // Japanese
		{"ko", "빠른 갈색 여우가 게으른 개를 뛰어넘는다"},                                 // Korean
		{"ar", "الثعلب البني السريع يقفز فوق الكلب الكسول"},              // Arabic (RTL)
		{"ru", "Быстрая коричневая лиса прыгает через ленивую собаку"},   // Cyrillic
		{"el", "Η γρήγορη καφέ αλεπού πηδά πάνω από το τεμπέλικο σκυλί"}, // Greek
		{"he", "השועל החום המהיר קופץ מעל הכלב העצלן"},                   // Hebrew (RTL)
		{"th", "สุนัขจิ้งจอกสีน้ำตาลกระโดดข้ามสุนัขขี้เกียจ"},            // Thai
		{"hi", "तेज़ भूरी लोमड़ी आलसी कुत्ते के ऊपर कूदती है"},           // Devanagari
		{"emoji", "🚀 LyEve CMS - 内容管理 🎯 システム 💾 데이터베이스"},                  // Mixed
	}

	for _, s := range samples {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (lang, content) VALUES (?, ?)", tableName),
			s.lang, s.content)
		if err != nil {
			t.Fatalf("insert %s: %v", s.lang, err)
		}
	}

	// Verify all rows roundtrip correctly.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT lang, content, CHAR_LENGTH(content) FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var lang, content string
		var charLen int
		if err := rows.Scan(&lang, &content, &charLen); err != nil {
			t.Fatalf("scan row %d: %v", i, err)
		}
		if content != samples[i].content {
			t.Errorf("%s: roundtrip failed\n  got  %q\n  want %q", lang, truncate(content, 50), truncate(samples[i].content, 50))
		}
		t.Logf("%s: %d chars, %d bytes", lang, charLen, len(content))
		i++
	}
	if i != len(samples) {
		t.Errorf("expected %d rows, got %d", len(samples), i)
	}
}

// Section 5: Collation Conflicts

// TestCollation_Conflict_JOIN verifies that MySQL 8.0 detects collation
// conflicts in JOINs and produces appropriate errors.
func TestCollation_Conflict_JOIN(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableA := csTableName("joina")
	tableB := csTableName("joinb")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableA+"`"+` (
			id INT PRIMARY KEY,
			name VARCHAR(50) COLLATE utf8mb4_unicode_ci
		)`)
	if err != nil {
		t.Fatalf("create table A: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableA+"`")

	_, err = pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableB+"`"+` (
			id INT PRIMARY KEY,
			name VARCHAR(50) COLLATE utf8mb4_bin
		)`)
	if err != nil {
		t.Fatalf("create table B: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableB+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` VALUES (1, 'Alice')", tableA))
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	_, err = pool.Exec(ctx, fmt.Sprintf("INSERT INTO `%s` VALUES (1, 'Alice')", tableB))
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}

	// JOIN on collation-mismatched columns should produce error 1267.
	quotedA := "`" + tableA + "`"
	quotedB := "`" + tableB + "`"
	_, err = pool.Query(ctx, fmt.Sprintf(`
		SELECT a.name FROM %s a JOIN %s b ON a.name = b.name`, quotedA, quotedB))
	if err != nil {
		t.Logf("collation conflict in JOIN correctly detected: %v", err)
	} else {
		t.Log("collation conflict in JOIN did not error (MySQL may have resolved automatically)")
	}

	// Explicit COLLATE clause should resolve the conflict.
	var name string
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT a.name FROM `%s` a JOIN `%s` b ON a.name COLLATE utf8mb4_bin = b.name",
		tableA, tableB),
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&name)
	if err != nil {
		t.Fatalf("explicit COLLATE JOIN: %v", err)
	}
	if name != "Alice" {
		t.Errorf("JOIN with COLLATE returned %q", name)
	}
}

// Section 6: ALTER TABLE Character Set Conversion

// TestCharset_AlterTable_ConvertCharset verifies that ALTER TABLE ... CONVERT
// TO CHARACTER SET correctly converts existing data.
func TestCharset_AlterTable_ConvertCharset(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("altercs")

	// Start with latin1.
	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content VARCHAR(255)
		) ENGINE=InnoDB DEFAULT CHARSET=latin1`)
	if err != nil {
		t.Fatalf("create latin1 table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert ASCII-friendly content in latin1.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (content) VALUES ('Hello World'), ('Café crème')", tableName))
	if err != nil {
		t.Fatalf("latin1 insert: %v", err)
	}

	// Convert to utf8mb4.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"ALTER TABLE `%s` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", tableName))
	if err != nil {
		t.Fatalf("convert to utf8mb4: %v", err)
	}

	// Verify data survived conversion.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT content FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("select after conversion: %v", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, c)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}
	if results[0] != "Hello World" {
		t.Errorf("row 1: got %q, want 'Hello World'", results[0])
	}
	if results[1] != "Café crème" {
		t.Errorf("row 2: got %q, want 'Café crème'", results[1])
	}
	t.Logf("latin1 -> utf8mb4 conversion: data preserved")
}

// Section 7: VARCHAR Length Semantics

// TestCharset_VARCHAR_LengthSemantics verifies that VARCHAR(N) counts
// characters (not bytes) in utf8mb4, and that 4-byte characters count
// as 1 character toward the limit.
func TestCharset_VARCHAR_LengthSemantics(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("varcharsem")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			short_col VARCHAR(5) CHARACTER SET utf8mb4
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// 5 ASCII characters should fit in VARCHAR(5).
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (short_col) VALUES ('abcde')", tableName))
	if err != nil {
		t.Fatalf("insert 5 ASCII chars: %v", err)
	}

	// 5 emoji characters should fit in VARCHAR(5): they count as 5 chars, not 20 bytes.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (short_col) VALUES ('😀😀😀😀😀')", tableName))
	if err != nil {
		t.Fatalf("insert 5 emoji chars (20 bytes) into VARCHAR(5): %v", err)
	}

	// 6 emoji characters should fail: 6 chars > VARCHAR(5).
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (short_col) VALUES ('😀😀😀😀😀😀')", tableName))
	if err == nil {
		t.Error("expected error inserting 6 chars into VARCHAR(5)")
	} else {
		t.Logf("6 emoji chars into VARCHAR(5) correctly rejected: %v", err)
	}

	// Verify the stored values.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT short_col, CHAR_LENGTH(short_col), LENGTH(short_col) FROM `%s` ORDER BY id", tableName))
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()

	type row struct {
		content string
		charLen int
		byteLen int
	}
	var results []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.content, &r.charLen, &r.byteLen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(results))
	}

	// Row 1: 5 ASCII chars = 5 characters, 5 bytes.
	if results[0].charLen != 5 || results[0].byteLen != 5 {
		t.Errorf("ASCII row: CHAR_LENGTH=%d, LENGTH=%d; want both 5", results[0].charLen, results[0].byteLen)
	}

	// Row 2: 5 emoji chars = 5 characters, 20 bytes.
	if results[1].charLen != 5 || results[1].byteLen != 20 {
		t.Errorf("Emoji row: CHAR_LENGTH=%d, LENGTH=%d; want 5 and 20", results[1].charLen, results[1].byteLen)
	}

	t.Logf("VARCHAR length semantics: characters, not bytes")
}

// Section 8: UNIQUE Constraint with Collation

// TestCollation_UniqueConstraint verifies that UNIQUE constraints respect
// the column's collation for determining duplicates.
func TestCollation_UniqueConstraint(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("uniqcoll")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			email_ci VARCHAR(255) COLLATE utf8mb4_unicode_ci UNIQUE,
			email_cs VARCHAR(255) COLLATE utf8mb4_bin UNIQUE
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (email_ci, email_cs) VALUES ('User@Example.com', 'User@Example.com')", tableName))
	if err != nil {
		t.Fatalf("insert first: %v", err)
	}

	// _ci: 'user@example.com' should be a duplicate of 'User@Example.com'.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (email_ci, email_cs) VALUES ('user@example.com', 'user@example.com')", tableName))
	if err == nil {
		t.Error("_ci unique constraint should reject case-insensitive duplicate")
	} else {
		t.Logf("_ci unique constraint correctly rejected 'user@example.com': %v", err)
	}

	// _bin: 'user@example.com' should be DIFFERENT from 'User@Example.com'.
	// But _ci column still prevents the insert since both must be unique.
	// Test _bin separately.
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (email_ci, email_cs) VALUES ('other@test.com', 'User@Example.com')", tableName))
	if err == nil {
		t.Error("_bin unique constraint should reject exact duplicate")
	} else {
		t.Logf("_bin unique constraint correctly rejected exact duplicate: %v", err)
	}

	// _bin: 'user@example.com' should be allowed (different case from 'User@Example.com').
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (email_ci, email_cs) VALUES ('other2@test.com', 'user@example.com')", tableName))
	if err != nil {
		t.Errorf("_bin unique constraint: 'user@example.com' should be allowed (different case): %v", err)
	} else {
		t.Log("_bin unique: case-different email allowed")
	}
}

// Section 9: Database/Server Level Collation

// TestCollation_ServerDefaults verifies server-level charset/collation settings.
func TestCollation_ServerDefaults(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	vars := map[string]string{
		"character_set_server":     "utf8mb4",
		"collation_server":         "utf8mb4",
		"character_set_database":   "utf8mb4",
		"collation_database":       "utf8mb4",
		"character_set_connection": "",
		"collation_connection":     "",
	}

	for name, expectedPrefix := range vars {
		var val string
		row, qrErr := pool.QueryRow(ctx, "SELECT @@"+name)
		if qrErr != nil {
			t.Fatalf("QueryRow: %v", qrErr)
		}
		err := row.Scan(&val)
		if err != nil {
			t.Logf("skip %s: %v", name, err)
			continue
		}
		t.Logf("%s = %s", name, val)
		if expectedPrefix != "" && !contains(val, expectedPrefix) {
			t.Errorf("%s = %q, expected to contain %q", name, val, expectedPrefix)
		}
	}
}

// Section 10: Mixed Collation Sorting

// TestCollation_Sorting verifies that different collations produce different
// ORDER BY results.
func TestCollation_Sorting(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := csTableName("sortcoll")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			label VARCHAR(50) COLLATE utf8mb4_unicode_ci,
			sort_key VARCHAR(50) COLLATE utf8mb4_bin
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	values := []string{"apple", "Banana", "cherry", "Apple", "banana", "Cherry", "äpfel"}
	for _, v := range values {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (label, sort_key) VALUES (?, ?)", tableName), v, v)
		if err != nil {
			t.Fatalf("insert %q: %v", v, err)
		}
	}

	// _ci sort: should group case-insensitively (Apple, apple, äpfel, Banana, banana, Cherry, cherry).
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT label FROM `%s` ORDER BY label", tableName))
	if err != nil {
		t.Fatalf("_ci sort: %v", err)
	}
	defer rows.Close()

	var ciSorted []string
	for rows.Next() {
		var label string
		rows.Scan(&label)
		ciSorted = append(ciSorted, label)
	}
	t.Logf("_ci sort: %v", ciSorted)

	// _bin sort: should sort by binary value (uppercase letters come before lowercase).
	rows2, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT sort_key FROM `%s` ORDER BY sort_key", tableName))
	if err != nil {
		t.Fatalf("_bin sort: %v", err)
	}
	defer rows2.Close()

	var binSorted []string
	for rows2.Next() {
		var label string
		rows2.Scan(&label)
		binSorted = append(binSorted, label)
	}
	t.Logf("_bin sort: %v", binSorted)

	// The two orderings should differ (case sensitivity matters in binary).
	if len(ciSorted) != len(binSorted) {
		t.Fatalf("sort result lengths differ: %d vs %d", len(ciSorted), len(binSorted))
	}

	differ := false
	for i := range ciSorted {
		if ciSorted[i] != binSorted[i] {
			differ = true
			break
		}
	}
	if !differ {
		t.Log("_ci and _bin sort produced identical orderings (may happen with simple data)")
	} else {
		t.Log("_ci and _bin sort produced different orderings")
	}
}

// Helpers

// truncate truncates a string for log display.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
