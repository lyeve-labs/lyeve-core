package db_test

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// SQL placeholder rewrite benchmarks: hot path on every query for MySQL/MSSQL

// These benchmarks exercise rewritePlaceholders which runs on every
// QueryRow/Query/Exec call for non-Postgres engines. Postgres is passthrough,
// but MySQL and MSSQL each get a full O(n) scan + replacement.

var (
	benchSimpleSQL   = "SELECT id, name, email FROM users WHERE id = $1"
	benchMultiArgSQL = "SELECT a, b, c, d, e, f, g, h, i, j FROM t WHERE a=$1 AND b=$2 AND c=$3 AND d=$4 AND e=$5 AND f=$6 AND g=$7 AND h=$8 AND i=$9 AND j=$10"
	benchInsertSQL   = "INSERT INTO items (id, slug, name, metadata, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id"
	benchComplexSQL  = "SELECT u.id, u.name, 'role: $1 is static' AS label, /* block comment $2 */ COUNT(*) OVER() AS total, -- line comment $3\n       u.email FROM users u WHERE u.tenant_id = $4 AND u.active = $5 ORDER BY u.name LIMIT $6"
)

// BenchmarkRewrite_Postgres_Passthrough, the fast path: no rewriting.
func BenchmarkRewrite_Postgres_Passthrough(b *testing.B) {
	q := benchMultiArgSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("postgres", q)
	}
}

// BenchmarkRewrite_MySQL_Simple measures a single placeholder in a short query.
func BenchmarkRewrite_MySQL_Simple(b *testing.B) {
	q := benchSimpleSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mysql", q)
	}
}

// BenchmarkRewrite_MySQL_MultiArg measures 10 placeholders, the common CRUD pattern.
func BenchmarkRewrite_MySQL_MultiArg(b *testing.B) {
	q := benchMultiArgSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mysql", q)
	}
}

// BenchmarkRewrite_MySQL_Insert measures an INSERT with RETURNING, a realistic plugin query.
func BenchmarkRewrite_MySQL_Insert(b *testing.B) {
	q := benchInsertSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mysql", q)
	}
}

// BenchmarkRewrite_MySQL_Comments measures mixed comments, strings and placeholders.
func BenchmarkRewrite_MySQL_Comments(b *testing.B) {
	q := benchComplexSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mysql", q)
	}
}

// BenchmarkRewrite_MSSQL_Simple measures the MSSQL named parameter path.
func BenchmarkRewrite_MSSQL_Simple(b *testing.B) {
	q := benchSimpleSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mssql", q)
	}
}

// BenchmarkRewrite_MSSQL_MultiArg measures MSSQL @pN generation, which is
// slightly heavier because it keeps the numbers.
func BenchmarkRewrite_MSSQL_MultiArg(b *testing.B) {
	q := benchMultiArgSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mssql", q)
	}
}

// BenchmarkRewrite_MSSQL_Comments measures the MSSQL comment-preserving rewrite.
func BenchmarkRewrite_MSSQL_Comments(b *testing.B) {
	q := benchComplexSQL
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = testRewrite("mssql", q)
	}
}

// testRewrite is a thin call-through so the compiler can't optimize away the
// result. We return a string to defeat dead-store elimination.
func testRewrite(engine, q string) string {
	s, _ := db.RewritePlaceholders(q, engine, nil)
	return s
}
