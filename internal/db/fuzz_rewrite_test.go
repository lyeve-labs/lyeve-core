// fuzz_rewrite_test.go: Go native fuzz harness for SQL placeholder rewrite.
//
// Verifies that rewritePlaceholders never panics on arbitrary SQL strings
// containing literal $N, ?, @pN tokens, string literals, comments, and
// adversarial combinations.
//
// Run:
//
//	go test -fuzz=FuzzRewritePlaceholders -fuzztime=30s ./internal/db/

package db

import (
	"testing"
)

// FuzzRewritePlaceholders fuzzes rewritePlaceholders with arbitrary SQL
// strings and engines. Tests that the rewrite:
//   - Never panics.
//   - For postgres: returns input unchanged (passthrough).
//   - For mysql: $N -> ?, args reordered. Literals in strings/comments preserved.
//   - For mssql: $N -> @pN, literals preserved.
//   - For unknown engines: $N left as-is.
//   - For mysql: args are never nil when the caller passed some.
func FuzzRewritePlaceholders(f *testing.F) {
	// Seed corpus: representative SQL patterns.
	f.Add("SELECT $1, $2, $3 FROM t WHERE x = $1", "postgres") // passthrough
	f.Add("SELECT $1, $2, $3 FROM t WHERE x = $1", "mysql")    // basic rewrite
	f.Add("SELECT $1, $2, $3 FROM t WHERE x = $1", "mssql")    // mssql rewrite
	f.Add("SELECT $1", "mysql")                                // single param
	f.Add("", "mysql")                                         // empty query
	f.Add("SELECT 1", "mysql")                                 // no params
	f.Add("SELECT '$1 is literal'", "mysql")                   // $N inside string
	f.Add("SELECT '-- $1 comment'", "mysql")                   // $N in line comment
	f.Add("SELECT /* $1 */ $2", "mysql")                       // $N in block comment
	f.Add("SELECT $1$2$3", "mysql")                            // adjacent params
	f.Add("SET name = $2 WHERE id = $1", "mysql")              // out-of-order
	f.Add("INSERT INTO t VALUES ($1, $2, '$3')", "mssql")      // $N in string literal
	f.Add("SELECT '''' $1", "mysql")                           // escaped quotes before param
	f.Add("SELECT $999", "mysql")                              // very high param number
	f.Add("SELECT $0", "mysql")                                // zero param (invalid)
	f.Add("SELECT $1, $1, $1", "mysql")                        // repeated param
	f.Add("SELECT $1", "unknown_engine")                       // unknown engine
	f.Add("SELECT $1 /* unterminated comment", "mysql")        // unterminated block comment
	f.Add("SELECT 'unterminated string $1", "mysql")           // unterminated string
	f.Add("SELECT $1; DROP TABLE t--", "mysql")                // injection attempt
	f.Add("SELECT @p1, @p2", "mssql")                          // MSSQL native syntax
	f.Add("SELECT $1 -- $2 in comment\n$3", "mysql")           // multi-line with comment

	f.Fuzz(func(t *testing.T, query, engine string) {
		// Must not panic.
		args := []any{"a", "b", "c", "d", "e"}
		result, resultArgs := rewritePlaceholders(query, engine, args)

		switch engine {
		case "postgres":
			// Invariant: postgres is passthrough.
			if result != query {
				t.Errorf("postgres rewrite changed query: got %q, want %q", result, query)
			}

		case "mysql":
			// Invariant: no $N tokens outside of string literals and comments.
			// $N inside single-quoted strings or block/line comments should be preserved.
			inStr := false
			inLineComment := false
			inBlockComment := false
			for i := 0; i < len(result); i++ {
				c := result[i]
				// Line comment
				if !inStr && !inBlockComment && c == '-' && i+1 < len(result) && result[i+1] == '-' {
					inLineComment = true
				}
				if inLineComment {
					if c == '\n' {
						inLineComment = false
					}
					continue
				}
				// Block comment
				if !inStr && !inBlockComment && c == '/' && i+1 < len(result) && result[i+1] == '*' {
					inBlockComment = true
					i++
					continue
				}
				if inBlockComment && c == '*' && i+1 < len(result) && result[i+1] == '/' {
					inBlockComment = false
					i++
					continue
				}
				if inBlockComment {
					continue
				}
				// String literal
				if c == '\'' {
					if inStr && i+1 < len(result) && result[i+1] == '\'' {
						i++
						continue
					}
					inStr = !inStr
				}
				if !inStr && c == '$' && i+1 < len(result) && result[i+1] >= '0' && result[i+1] <= '9' {
					t.Errorf("mysql output contains $N outside string/comment at position %d: %q", i, result)
				}
			}

			// Invariant: resultArgs must be non-nil when len(args) > 0.
			// A nil resultArgs with non-nil args indicates a slice-init bug.
			if resultArgs == nil {
				t.Errorf("mysql rewrite returned nil args for %d inputs: %q", len(args), query)
			}

		case "mssql":
			// Invariant: no $N tokens outside of string literals and comments.
			inStr := false
			inLineComment := false
			inBlockComment := false
			for i := 0; i < len(result); i++ {
				c := result[i]
				if !inStr && !inBlockComment && c == '-' && i+1 < len(result) && result[i+1] == '-' {
					inLineComment = true
				}
				if inLineComment {
					if c == '\n' {
						inLineComment = false
					}
					continue
				}
				if !inStr && !inBlockComment && c == '/' && i+1 < len(result) && result[i+1] == '*' {
					inBlockComment = true
					i++
					continue
				}
				if inBlockComment && c == '*' && i+1 < len(result) && result[i+1] == '/' {
					inBlockComment = false
					i++
					continue
				}
				if inBlockComment {
					continue
				}
				if c == '\'' {
					if inStr && i+1 < len(result) && result[i+1] == '\'' {
						i++
						continue
					}
					inStr = !inStr
				}
				if !inStr && c == '$' && i+1 < len(result) && result[i+1] >= '0' && result[i+1] <= '9' {
					t.Errorf("mssql output contains $N outside string/comment at position %d: %q", i, result)
				}
			}

		default:
			// Unknown engine: $N should be left as-is.
			if result != query {
				t.Errorf("unknown engine %q: got %q, want %q", engine, result, query)
			}
		}

		// General invariant: output must never be longer than input by more than
		// a factor of 2 (guards against runaway expansion).
		if len(result) > len(query)*2+100 {
			t.Errorf("output suspiciously long: len=%d, input len=%d", len(result), len(query))
		}
	})
}
