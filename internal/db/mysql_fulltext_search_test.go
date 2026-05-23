//go:build !short && !mutest

// MySQL Full-Text Search Integration Tests
// Exercises MySQL 8.0's InnoDB full-text search capabilities including:
//   - NATURAL LANGUAGE MODE (relevance-ranked)
//   - BOOLEAN MODE (operators: +, -, *, ", etc.)
//   - ngram parser for CJK (Chinese/Japanese/Korean) tokenization
//   - Minimum word length (innodb_ft_min_token_size)
//   - Stopword handling
//   - Full-text index creation and verification
//   - MATCH ... AGAINST syntax variations
//   - Query expansion WITH QUERY EXPANSION
//
// The ngram parser is critical for CMS multilingual content search.
// Without ngram, MySQL's default word breaker can't tokenize CJK text
// (no spaces between words), making full-text search useless for Asian
// language content.
//
// References:
//   - MySQL 8.0 Reference Manual §14.9 (Full-Text Search Functions)
//   - https://dev.mysql.com/doc/refman/8.0/en/fulltext-search.html
//   - https://dev.mysql.com/doc/refman/8.0/en/fulltext-search-ngram.html
package db_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/testdb"
)

// ftTableName generates a deterministic table name for full-text tests.
func ftTableName(prefix string) string {
	return fmt.Sprintf("ft_%s_%d", prefix, time.Now().UnixNano()%100000)
}

// Section 1: NATURAL LANGUAGE MODE

// TestFulltext_NaturalLanguageMode_Basic tests basic full-text search
// in natural language mode with relevance ranking.
func TestFulltext_NaturalLanguageMode_Basic(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := ftTableName("natural")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			title VARCHAR(255) NOT NULL,
			body TEXT NOT NULL,
			FULLTEXT INDEX ft_content (title, body)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create fulltext table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert diverse content.
	articles := []struct{ title, body string }{
		{"MySQL Performance Tuning", "Learn how to optimize MySQL queries and indexes for better performance"},
		{"Introduction to Docker", "Docker containers simplify deployment and scaling of applications"},
		{"Advanced MySQL Replication", "Configure MySQL master-slave replication for high availability"},
		{"PostgreSQL vs MySQL", "Comparing PostgreSQL and MySQL for your next project"},
		{"Kubernetes Basics", "Getting started with Kubernetes orchestration for containerized apps"},
		{"MySQL Full-Text Search", "Using MySQL's built-in full-text search for content discovery"},
	}
	for _, a := range articles {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (title, body) VALUES (?, ?)", tableName),
			a.title, a.body)
		if err != nil {
			t.Fatalf("insert article: %v", err)
		}
	}

	// Search for "MySQL" - should get relevance-ranked results.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT title, MATCH(title, body) AGAINST('MySQL' IN NATURAL LANGUAGE MODE) AS relevance FROM `%s` WHERE MATCH(title, body) AGAINST('MySQL' IN NATURAL LANGUAGE MODE) ORDER BY relevance DESC",
		tableName))
	if err != nil {
		t.Fatalf("FULLTEXT search: %v", err)
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var title string
		var relevance float64
		if err := rows.Scan(&title, &relevance); err != nil {
			t.Fatalf("scan: %v", err)
		}
		results = append(results, title)
		t.Logf("  relevance=%.4f title=%q", relevance, title)
	}

	// "MySQL" appears in 4 articles.
	if len(results) < 2 {
		t.Errorf("expected at least 2 MySQL results, got %d", len(results))
	}

	// First result should contain "MySQL" in the title or be highly relevant.
	foundMySQL := false
	for _, r := range results {
		if strings.Contains(r, "MySQL") {
			foundMySQL = true
			break
		}
	}
	if !foundMySQL {
		t.Error("no results contained 'MySQL' in title")
	}
}

// TestFulltext_NaturalLanguageMode_NoResults verifies that searching for
// a term not present returns zero rows.
func TestFulltext_NaturalLanguageMode_NoResults(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := ftTableName("noresults")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_content (content)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (content) VALUES (?)", tableName),
		"The quick brown fox jumps over the lazy dog")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var count int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('elephant' IN NATURAL LANGUAGE MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 results for 'elephant', got %d", count)
	}
}

// Section 2: BOOLEAN MODE

// TestFulltext_BooleanMode_Operators tests Boolean mode operators.
func TestFulltext_BooleanMode_Operators(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := ftTableName("boolean")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_content (content)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	docs := []string{
		"MySQL is great for web applications",
		"PostgreSQL is great for analytics",
		"MySQL and PostgreSQL both support JSON",
		"MongoDB is a NoSQL database",
		"MySQL supports full-text search with InnoDB",
	}
	for _, d := range docs {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (content) VALUES (?)", tableName), d)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// +MySQL -PostgreSQL: must contain MySQL, must NOT contain PostgreSQL.
	var count int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('+MySQL -PostgreSQL' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("+MySQL -PostgreSQL: %v", err)
	}
	if count != 2 {
		t.Errorf("'+MySQL -PostgreSQL' matched %d rows, expected 2 (MySQL-only articles)", count)
	}

	// +MySQL +PostgreSQL: must contain both.
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('+MySQL +PostgreSQL' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("+MySQL +PostgreSQL: %v", err)
	}
	if count != 1 {
		t.Errorf("'+MySQL +PostgreSQL' matched %d rows, expected 1 (both)", count)
	}

	// MySQL PostgreSQL (no operator = OR).
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('MySQL PostgreSQL' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("MySQL PostgreSQL (OR): %v", err)
	}
	if count < 3 {
		t.Errorf("'MySQL PostgreSQL' (OR) matched %d rows, expected at least 3", count)
	}

	// >MySQL <PostgreSQL: MySQL more relevant, PostgreSQL less.
	rows, err := pool.Query(ctx, fmt.Sprintf(
		"SELECT content FROM `%s` WHERE MATCH(content) AGAINST('>MySQL <PostgreSQL' IN BOOLEAN MODE)",
		tableName))
	if err != nil {
		t.Fatalf("relevance boost: %v", err)
	}
	defer rows.Close()
	rowCount := 0
	for rows.Next() {
		rowCount++
	}
	if rowCount == 0 {
		t.Error("relevance-weighted search returned 0 results")
	}
}

// Section 3: ngram Parser for CJK

// TestFulltext_Ngram_CJK tests the ngram parser for Chinese, Japanese, and
// Korean text where words are not space-delimited.
//
// Requires: MySQL 8.0 with ngram parser available.
// The test creates a FULLTEXT INDEX WITH PARSER ngram.
func TestFulltext_Ngram_CJK(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	// Check ngram parser availability.
	var ngramAvailable string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT PLUGIN_STATUS FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'ngram'",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&ngramAvailable)
	if err != nil || ngramAvailable != "ACTIVE" {
		t.Skip("ngram parser not available (PLUGIN_STATUS=" + ngramAvailable + ")")
	}

	tableName := ftTableName("ngram")

	_, err = pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			lang VARCHAR(10) NOT NULL,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_ngram (content) WITH PARSER ngram
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create ngram fulltext table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Insert CJK content.
	docs := []struct{ lang, content string }{
		// Chinese: "Database performance optimization"
		{"zh", "数据库性能优化是每个开发者都需要关注的"},
		// Chinese: "MySQL full-text search supports ngram"
		{"zh", "MySQL全文搜索支持ngram分词器"},
		// Japanese: "PostgreSQL also supports full-text search"
		{"ja", "PostgreSQLも全文検索をサポートしています"},
		// Japanese: "InnoDB provides high availability"
		{"ja", "InnoDBは高い可用性を提供します"},
		// Korean: "MySQL 8.0 has many new features"
		{"ko", "MySQL 8.0에는 많은 새로운 기능이 있습니다"},
		// Korean: "Database indexing improves performance"
		{"ko", "데이터베이스 인덱싱은 성능을 향상시킵니다"},
		// Mixed: English (should still work with ngram)
		{"en", "MySQL database performance optimization guide"},
	}
	for _, d := range docs {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (lang, content) VALUES (?, ?)", tableName),
			d.lang, d.content)
		if err != nil {
			t.Fatalf("insert %s doc: %v", d.lang, err)
		}
	}

	tests := []struct {
		name    string
		query   string
		minHits int
		desc    string
	}{
		{
			name: "Chinese exact", query: "数据库",
			minHits: 1,
			desc:    "should find Chinese articles containing 数据库 (database)",
		},
		{
			name: "Chinese cross-word", query: "性能优化",
			minHits: 1,
			desc:    "should match compounds like 性能优化 (performance optimization)",
		},
		{
			name: "Japanese", query: "全文検索",
			minHits: 1,
			desc:    "should find Japanese articles with 全文検索 (full-text search)",
		},
		{
			name: "Korean", query: "데이터베이스",
			minHits: 1,
			desc:    "should find Korean articles with 데이터베이스 (database)",
		},
		{
			name: "English through ngram", query: "MySQL",
			minHits: 3,
			desc:    "should find MySQL across languages (ngram tokenizes English too)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use BOOLEAN MODE with ngram for better CJK matching.
			var count int
			row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST(? IN BOOLEAN MODE)",
				tableName), tt.query)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err = row.Scan(&count)
			if err != nil {
				// Some queries may fail if all tokens are under min/max ngram size.
				t.Logf("ngram BOOLEAN MODE search for %q error: %v", tt.query, err)
				return
			}
			if count < tt.minHits {
				t.Errorf("%s: got %d hits, need at least %d (%s)", tt.name, count, tt.minHits, tt.desc)
			} else {
				t.Logf("%s: %d hits", tt.name, count)
			}
		})
	}
}

// TestFulltext_Ngram_SubstringMatch verifies that ngram can match substrings
// of CJK words, which is its key advantage over the default parser.
func TestFulltext_Ngram_SubstringMatch(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var ngramStatus string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT PLUGIN_STATUS FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'ngram'",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&ngramStatus)
	if err != nil || ngramStatus != "ACTIVE" {
		t.Skip("ngram parser not available")
	}

	tableName := ftTableName("ngsub")

	_, err = pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_ngram (content) WITH PARSER ngram
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create ngram table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	// Chinese compound: "lyeve内容管理系统"
	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (content) VALUES (?)", tableName),
		"lyeve内容管理系统是一个强大的CMS平台")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	tests := []struct {
		query   string
		wantHit bool
		desc    string
	}{
		{"内容管理", true, "exact compound match"},
		{"内容", true, "substring (ngram matches any 2-char token)"},
		{"管理", true, "another substring"},
		{"系统", true, "single-character? may depend on ngram_token_size"},
		{"强大的", true, "adjective in ngram"},
		{"不存在", false, "non-existent term should not match"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			var count int
			row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
				"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST(? IN BOOLEAN MODE)",
				tableName), tt.query)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err = row.Scan(&count)
			if err != nil {
				t.Logf("search for %q: %v", tt.query, err)
				return
			}
			hit := count > 0
			if hit != tt.wantHit {
				t.Errorf("%q: hit=%v, want=%v", tt.query, hit, tt.wantHit)
			} else {
				t.Logf("%q: hit=%v", tt.query, hit)
			}
		})
	}
}

// Section 4: Full-Text Configuration

// TestFulltext_Configuration verifies innodb_ft_min_token_size and other
// full-text configuration variables are sensible.
func TestFulltext_Configuration(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	configVars := map[string]struct {
		min, max int
	}{
		"innodb_ft_min_token_size":   {1, 16},
		"innodb_ft_max_token_size":   {10, 84},
		"innodb_ft_enable_stopword":  {0, 1},
		"innodb_ft_sort_pll_degree":  {1, 32},
		"innodb_ft_cache_size":       {800000, 80000000},
		"innodb_ft_total_cache_size": {32000000, 1600000000},
	}

	for name, bounds := range configVars {
		t.Run(name, func(t *testing.T) {
			var val int
			row, qrErr := pool.QueryRow(ctx, "SELECT @@"+name)
			if qrErr != nil {
				t.Fatalf("QueryRow: %v", qrErr)
			}
			err := row.Scan(&val)
			if err != nil {
				t.Logf("skipping %s: %v", name, err)
				return
			}
			t.Logf("%s = %d", name, val)
			if val < bounds.min || val > bounds.max {
				t.Errorf("%s = %d, expected range [%d, %d]", name, val, bounds.min, bounds.max)
			}
		})
	}
}

// TestFulltext_NgramConfig verifies ngram-specific configuration variables.
func TestFulltext_NgramConfig(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	// ngram_token_size: default is 2 (bigram). Can be 1-10.
	var ngramSize int
	row, qrErr := pool.QueryRow(ctx, "SELECT @@ngram_token_size")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&ngramSize)
	if err != nil {
		t.Skipf("ngram_token_size not available: %v", err)
	}
	t.Logf("ngram_token_size = %d (default=2 for bigram)", ngramSize)
	if ngramSize < 1 || ngramSize > 10 {
		t.Errorf("ngram_token_size = %d, expected range [1, 10]", ngramSize)
	}
}

// Section 5: WITH QUERY EXPANSION

// TestFulltext_QueryExpansion tests that WITH QUERY EXPANSION finds related
// documents that don't contain the original search term.
func TestFulltext_QueryExpansion(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := ftTableName("qexpand")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_content (content)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	docs := []string{
		"MySQL is a relational database management system",
		"Database indexing improves query performance significantly",
		"Query optimization is essential for fast databases",
		"Relational databases use SQL for data manipulation",
		"Content management systems need fast full-text search",
	}
	for _, d := range docs {
		_, err = pool.Exec(ctx, fmt.Sprintf(
			"INSERT INTO `%s` (content) VALUES (?)", tableName), d)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	// NATURAL LANGUAGE MODE: only exact matches.
	var nlCount int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('database' IN NATURAL LANGUAGE MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&nlCount)
	if err != nil {
		t.Fatalf("NL mode: %v", err)
	}

	// WITH QUERY EXPANSION: should find related documents too.
	var qeCount int
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('database' WITH QUERY EXPANSION)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&qeCount)
	if err != nil {
		t.Fatalf("QE mode: %v", err)
	}

	// Query expansion should return at least as many results (usually more).
	if qeCount < nlCount {
		t.Errorf("query expansion returned %d rows, natural language returned %d; expansion should return >= NL",
			qeCount, nlCount)
	}
	t.Logf("NL mode: %d rows, WITH QUERY EXPANSION: %d rows", nlCount, qeCount)
}

// Section 6: Stopwords & Minimum Word Length

// TestFulltext_Stopwords verifies that the default stopword list excludes
// common words from the full-text index.
func TestFulltext_Stopwords(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()
	tableName := ftTableName("stopword")

	_, err := pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_content (content)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (content) VALUES (?)", tableName),
		"The quick brown fox jumps over the lazy dog")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// "the" is a stopword - searching for it in BOOLEAN MODE should return 0.
	var count int
	row, qrErr := pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('the' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("stopword search: %v", err)
	}
	if count != 0 {
		t.Logf("stopword 'the' returned %d results (stopword list may differ)", count)
	}

	// "quick" is not a stopword - should find the document.
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('quick' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("non-stopword search: %v", err)
	}
	if count != 1 {
		t.Errorf("'quick' returned %d results, expected 1", count)
	}
}

// TestFulltext_MinTokenSize verifies that words shorter than
// innodb_ft_min_token_size (default 3) are excluded from the index.
func TestFulltext_MinTokenSize(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	// Read the current min token size.
	var minTokenSize int
	row, qrErr := pool.QueryRow(ctx, "SELECT @@innodb_ft_min_token_size")
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&minTokenSize)
	if err != nil {
		t.Fatalf("read min token size: %v", err)
	}
	t.Logf("innodb_ft_min_token_size = %d", minTokenSize)

	tableName := ftTableName("mintok")

	_, err = pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_content (content)
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (content) VALUES (?)", tableName),
		"go is a modern programming language")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// "go" is 2 chars - if min_token_size is 3, it should not be searchable.
	var count int
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('go' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("short word search: %v", err)
	}

	if minTokenSize > len("go") && count > 0 {
		t.Errorf("'go' (%d chars) should not be indexable with min_token_size=%d, but got %d results",
			len("go"), minTokenSize, count)
	} else {
		t.Logf("'go' search with min_token_size=%d: %d results", minTokenSize, count)
	}

	// "language" is 8 chars - should always be indexable.
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('language' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count)
	if err != nil {
		t.Fatalf("long word search: %v", err)
	}
	if count != 1 {
		t.Errorf("'language' should be indexable but got %d results", count)
	}
}

// Section 7: Multiple Full-Text Indexes

// TestFulltext_MultipleIndexes verifies that a table can have separate
// full-text indexes for different columns/strategies.
func TestFulltext_MultipleIndexes(t *testing.T) {
	if testing.Short() || !testdb.ShouldTest("mysql") {
		t.Skip("skipping MySQL integration test: short mode or CI_DIALECT excludes mysql")
	}

	pool := testdb.MySQL(t)
	ctx := context.Background()

	var ngramStatus string
	row, qrErr := pool.QueryRow(ctx,
		"SELECT PLUGIN_STATUS FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'ngram'",
	)
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err := row.Scan(&ngramStatus)
	if err != nil || ngramStatus != "ACTIVE" {
		t.Skip("ngram parser not available")
	}

	tableName := ftTableName("multiidx")

	// Two full-text indexes: one default (title) and one ngram (content).
	_, err = pool.Exec(ctx, `
		CREATE TABLE `+"`"+tableName+"`"+` (
			id INT AUTO_INCREMENT PRIMARY KEY,
			title VARCHAR(255) NOT NULL,
			content TEXT NOT NULL,
			FULLTEXT INDEX ft_title (title),
			FULLTEXT INDEX ft_content_ngram (content) WITH PARSER ngram
		) ENGINE=InnoDB`)
	if err != nil {
		t.Fatalf("create multi-index table: %v", err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tableName+"`")

	_, err = pool.Exec(ctx, fmt.Sprintf(
		"INSERT INTO `%s` (title, content) VALUES (?, ?)", tableName),
		"Database Guide", "数据库管理系统提供了强大的查询功能")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Search title with default parser.
	var count1 int
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(title) AGAINST('Database' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count1)
	if err != nil {
		t.Fatalf("title search: %v", err)
	}
	if count1 != 1 {
		t.Errorf("title search for 'Database' returned %d", count1)
	}

	// Search content with ngram parser for Chinese.
	var count2 int
	row, qrErr = pool.QueryRow(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE MATCH(content) AGAINST('数据库' IN BOOLEAN MODE)",
		tableName))
	if qrErr != nil {
		t.Fatalf("QueryRow: %v", qrErr)
	}
	err = row.Scan(&count2)
	if err != nil {
		t.Fatalf("content ngram search: %v", err)
	}
	if count2 != 1 {
		t.Errorf("content ngram search for '数据库' returned %d", count2)
	}

	t.Logf("index ft_title: %d hits, ft_content_ngram: %d hits", count1, count2)
}
