// pbt_rewrite_test.go: Property-based tests for SQL placeholder rewrite.
//
// Encodes cross-dialect invariants using pgregory.net/rapid:
//   - Arg count after rewrite always matches placeholder count.
//   - $N inside string literals and comments are never rewritten.
//   - Injection-safe: no unescaped $N leaks through on MySQL/MSSQL.
//
// Run:
//
//	go test -race -run TestPBT ./internal/db/

package db

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// placeholderCount counts the number of positional placeholders in the output SQL.
//   - MySQL: count `?` outside strings/comments
//   - MSSQL: count `@pN` outside strings/comments
func placeholderCount(sql, engine string) int {
	count := 0
	inStr := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		// Line comment
		if !inStr && !inBlockComment && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
		}
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}

		// Block comment
		if !inStr && !inBlockComment && c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if inBlockComment && c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			inBlockComment = false
			i++
			continue
		}
		if inBlockComment {
			continue
		}

		// String literal
		if c == '\'' {
			if inStr && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}

		switch engine {
		case "mysql":
			if c == '?' {
				count++
			}
		case "mssql":
			if c == '@' && i+2 < len(sql) && sql[i+1] == 'p' && sql[i+2] >= '0' && sql[i+2] <= '9' {
				count++
				// skip past the number
				j := i + 3
				for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
					j++
				}
				i = j - 1
			}
		}
	}
	return count
}

// maxPlaceholderIndex extracts the highest $N index from a SQL string
// (outside strings/comments). Returns 0 if none found.
func maxPlaceholderIndex(sql string) int {
	maxN := 0
	inStr := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		if !inStr && !inBlockComment && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
		}
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}

		if !inStr && !inBlockComment && c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if inBlockComment && c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			inBlockComment = false
			i++
			continue
		}
		if inBlockComment {
			continue
		}

		if c == '\'' {
			if inStr && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}

		if c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
			n := 0
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				n = n*10 + int(sql[j]-'0')
				j++
			}
			if n > maxN {
				maxN = n
			}
			i = j - 1
		}
	}
	return maxN
}

// countDollarPlaceholders counts distinct $N occurrences (outside strings/comments).
func countDollarPlaceholders(sql string) int {
	count := 0
	inStr := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		if !inStr && !inBlockComment && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
		}
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}

		if !inStr && !inBlockComment && c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if inBlockComment && c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			inBlockComment = false
			i++
			continue
		}
		if inBlockComment {
			continue
		}

		if c == '\'' {
			if inStr && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}

		if c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
			count++
			j := i + 2
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			i = j - 1
		}
	}
	return count
}

// --- Generators ---

// genSQLWithPlaceholders generates SQL containing random $N placeholders,
// string literals, comments, and plain text fragments.
func genSQLWithPlaceholders() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		var b strings.Builder
		numFragments := rapid.IntRange(1, 8).Draw(t, "numFragments")
		for i := 0; i < numFragments; i++ {
			choice := rapid.IntRange(0, 5).Draw(t, "kind")
			switch choice {
			case 0: // plain text
				b.WriteString(rapid.StringMatching(`[A-Za-z_][A-Za-z0-9_ .,=<>!]*`).Draw(t, "text"))
			case 1: // $N placeholder
				n := rapid.IntRange(1, 20).Draw(t, "n")
				fmt.Fprintf(&b, "$%d", n)
			case 2: // string literal containing $N (should be preserved)
				n := rapid.IntRange(1, 20).Draw(t, "n")
				fmt.Fprintf(&b, "'value $%d here'", n)
			case 3: // line comment containing $N (should be preserved)
				n := rapid.IntRange(1, 20).Draw(t, "n")
				fmt.Fprintf(&b, "-- comment $%d\n", n)
			case 4: // block comment containing $N (should be preserved)
				n := rapid.IntRange(1, 20).Draw(t, "n")
				fmt.Fprintf(&b, "/* block $%d */", n)
			case 5: // adjacent placeholders
				n1 := rapid.IntRange(1, 10).Draw(t, "n1")
				n2 := rapid.IntRange(1, 10).Draw(t, "n2")
				fmt.Fprintf(&b, "$%d$%d", n1, n2)
			}
		}
		return b.String()
	})
}

// genEngine returns a random dialect engine name.
func genEngine() *rapid.Generator[string] {
	return rapid.OneOf(
		rapid.Just("postgres"),
		rapid.Just("mysql"),
		rapid.Just("mssql"),
	)
}

// genArgs returns a random-length args slice.
func genArgs() *rapid.Generator[[]any] {
	return rapid.Custom(func(t *rapid.T) []any {
		n := rapid.IntRange(0, 20).Draw(t, "n")
		args := make([]any, n)
		for i := range args {
			args[i] = rapid.String().Draw(t, fmt.Sprintf("arg_%d", i))
		}
		return args
	})
}

// --- Properties ---

// Property: PostgreSQL rewrite is always a passthrough.
func TestPBT_PostgresIsPassthrough(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		args := genArgs().Draw(t, "args")
		got, gotArgs := rewritePlaceholders(sql, "postgres", args)
		if got != sql {
			t.Errorf("postgres not passthrough: got %q, want %q", got, sql)
		}
		if len(gotArgs) != len(args) {
			t.Errorf("postgres args changed: got len %d, want %d", len(gotArgs), len(args))
		}
	})
}

// Property: After MySQL rewrite, the number of `?` placeholders in the output
// equals the number of $N placeholders outside strings/comments in the input.
func TestPBT_MySQLPlaceholderCountMatches(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		args := genArgs().Draw(t, "args")

		dollarCount := countDollarPlaceholders(sql)
		_, gotArgs := rewritePlaceholders(sql, "mysql", args)
		rewritten, _ := rewritePlaceholders(sql, "mysql", args)
		questionCount := placeholderCount(rewritten, "mysql")

		if questionCount != dollarCount {
			t.Errorf("placeholder count mismatch: input has %d $N, output has %d ? (sql=%q, rewritten=%q)",
				dollarCount, questionCount, sql, rewritten)
		}

		// The returned args length should match the number of $N in input
		// (if all $N are valid 1-based indices within args range).
		maxN := maxPlaceholderIndex(sql)
		if maxN > 0 && maxN <= len(args) {
			if len(gotArgs) != dollarCount {
				t.Errorf("mysql arg count: got %d, want %d (dollarCount=%d, maxN=%d, argsLen=%d, sql=%q)",
					len(gotArgs), dollarCount, dollarCount, maxN, len(args), sql)
			}
		}
	})
}

// Property: After MSSQL rewrite, the number of @pN placeholders in the output
// equals the number of $N outside strings/comments in the input.
func TestPBT_MSSQLPlaceholderCountMatches(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")

		dollarCount := countDollarPlaceholders(sql)
		rewritten, _ := rewritePlaceholders(sql, "mssql", nil)
		atpCount := placeholderCount(rewritten, "mssql")

		if atpCount != dollarCount {
			t.Errorf("mssql placeholder count mismatch: input has %d $N, output has %d @pN (sql=%q, rewritten=%q)",
				dollarCount, atpCount, sql, rewritten)
		}
	})
}

// Property: $N inside single-quoted string literals are NEVER rewritten
// on any dialect.
func TestPBT_StringLiteralsPreserved(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")

		// Find all $N inside string literals in the input
		var literalDollars []string
		inStr := false
		for i := 0; i < len(sql); i++ {
			c := sql[i]
			if c == '\'' {
				if inStr && i+1 < len(sql) && sql[i+1] == '\'' {
					i++
					continue
				}
				inStr = !inStr
				continue
			}
			if inStr && c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
				j := i + 1
				for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
					j++
				}
				literalDollars = append(literalDollars, sql[i:j])
				i = j - 1
			}
		}

		// Each literal $N must appear unchanged in the rewritten output
		for _, engine := range []string{"mysql", "mssql"} {
			rewritten, _ := rewritePlaceholders(sql, engine, nil)
			for _, dollar := range literalDollars {
				// Check that the $N appears inside a string literal in the output
				// by looking for the pattern '...$N...'
				needle := dollar
				if !strings.Contains(rewritten, needle) {
					// The $N could have been consumed by the rewrite if it appeared
					// outside a string too. Check more carefully: scan the output
					// for $N inside strings.
					found := false
					outInStr := false
					for k := 0; k < len(rewritten); k++ {
						cc := rewritten[k]
						if cc == '\'' {
							if outInStr && k+1 < len(rewritten) && rewritten[k+1] == '\'' {
								k++
								continue
							}
							outInStr = !outInStr
							continue
						}
						if outInStr && k+len(needle) <= len(rewritten) && rewritten[k:k+len(needle)] == needle {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("%s: literal %q not preserved in output %q (input %q)", engine, dollar, rewritten, sql)
					}
				}
			}
		}
	})
}

// Property: On MySQL, no $N digit sequence appears outside string literals or
// comments in the output (injection safety).
func TestPBT_MySQLNoDollarNOutsideLiterals(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		rewritten, _ := rewritePlaceholders(sql, "mysql", nil)

		inStr := false
		inLineComment := false
		inBlockComment := false

		for i := 0; i < len(rewritten); i++ {
			c := rewritten[i]

			if !inStr && !inBlockComment && c == '-' && i+1 < len(rewritten) && rewritten[i+1] == '-' {
				inLineComment = true
			}
			if inLineComment {
				if c == '\n' {
					inLineComment = false
				}
				continue
			}

			if !inStr && !inBlockComment && c == '/' && i+1 < len(rewritten) && rewritten[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
			if inBlockComment && c == '*' && i+1 < len(rewritten) && rewritten[i+1] == '/' {
				inBlockComment = false
				i++
				continue
			}
			if inBlockComment {
				continue
			}

			if c == '\'' {
				if inStr && i+1 < len(rewritten) && rewritten[i+1] == '\'' {
					i++
					continue
				}
				inStr = !inStr
				continue
			}
			if inStr {
				continue
			}

			if c == '$' && i+1 < len(rewritten) && rewritten[i+1] >= '0' && rewritten[i+1] <= '9' {
				t.Errorf("mysql: found $N outside literal/comment at pos %d in %q (from %q)", i, rewritten, sql)
			}
		}
	})
}

// Property: On MSSQL, no $N digit sequence appears outside string literals or
// comments in the output (injection safety).
func TestPBT_MSSQLNoDollarNOutsideLiterals(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		rewritten, _ := rewritePlaceholders(sql, "mssql", nil)

		inStr := false
		inLineComment := false
		inBlockComment := false

		for i := 0; i < len(rewritten); i++ {
			c := rewritten[i]

			if !inStr && !inBlockComment && c == '-' && i+1 < len(rewritten) && rewritten[i+1] == '-' {
				inLineComment = true
			}
			if inLineComment {
				if c == '\n' {
					inLineComment = false
				}
				continue
			}

			if !inStr && !inBlockComment && c == '/' && i+1 < len(rewritten) && rewritten[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
			if inBlockComment && c == '*' && i+1 < len(rewritten) && rewritten[i+1] == '/' {
				inBlockComment = false
				i++
				continue
			}
			if inBlockComment {
				continue
			}

			if c == '\'' {
				if inStr && i+1 < len(rewritten) && rewritten[i+1] == '\'' {
					i++
					continue
				}
				inStr = !inStr
				continue
			}
			if inStr {
				continue
			}

			if c == '$' && i+1 < len(rewritten) && rewritten[i+1] >= '0' && rewritten[i+1] <= '9' {
				t.Errorf("mssql: found $N outside literal/comment at pos %d in %q (from %q)", i, rewritten, sql)
			}
		}
	})
}

// Property: Output length is bounded, no runaway expansion.
func TestPBT_OutputLengthBounded(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		for _, engine := range []string{"postgres", "mysql", "mssql"} {
			rewritten, _ := rewritePlaceholders(sql, engine, nil)
			// MSSQL @pN can expand $N (3 extra chars per placeholder), so allow for that.
			// Generous bound: input length * 3 + 100.
			if len(rewritten) > len(sql)*3+100 {
				t.Errorf("%s: output len %d exceeds bound for input len %d (sql=%q, rewritten=%q)",
					engine, len(rewritten), len(sql), sql, rewritten)
			}
		}
	})
}

// Property: MySQL arg reordering preserves the set of arg values. No values
// are dropped or duplicated.
func TestPBT_MySQLArgSetPreserved(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		args := genArgs().Draw(t, "args")

		dollarCount := countDollarPlaceholders(sql)
		_, gotArgs := rewritePlaceholders(sql, "mysql", args)

		// If all $N are valid (1..len(args)), the output args must be a
		// permutation (subset with ordering) of the input args.
		maxN := maxPlaceholderIndex(sql)
		if maxN > 0 && maxN <= len(args) && dollarCount > 0 {
			if len(gotArgs) != dollarCount {
				t.Errorf("mysql: expected %d args, got %d", dollarCount, len(gotArgs))
			}
			// Each returned arg must be one of the original args
			for i, a := range gotArgs {
				found := false
				for _, orig := range args {
					if fmt.Sprintf("%v", a) == fmt.Sprintf("%v", orig) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("mysql: gotArgs[%d]=%v not found in original args %v (sql=%q)", i, a, args, sql)
				}
			}
		}
	})
}

// Property: rewrite never panics on any input (safety net). The fuzz test also
// covers this, but PBT gives us shrinking.
func TestPBT_NeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		engine := genEngine().Draw(t, "engine")
		args := genArgs().Draw(t, "args")
		rewritePlaceholders(sql, engine, args)
	})
}

// Property: MSSQL @pN numbers in the output correspond 1:1 to $N in the input
// (outside strings/comments), preserving order.
func TestPBT_MSSQLPlaceholderOrderPreserved(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sql := genSQLWithPlaceholders().Draw(t, "sql")
		rewritten, _ := rewritePlaceholders(sql, "mssql", nil)

		// Extract $N numbers from input (outside strings/comments)
		inputNums := extractPlaceholderNumbers(sql)
		// Extract @pN numbers from output (outside strings/comments)
		outputNums := extractAtPNNumbers(rewritten)

		if len(inputNums) != len(outputNums) {
			t.Errorf("mssql: input has %d $N, output has %d @pN (sql=%q, rewritten=%q)",
				len(inputNums), len(outputNums), sql, rewritten)
			return
		}
		for i := range inputNums {
			if inputNums[i] != outputNums[i] {
				t.Errorf("mssql: placeholder %d mismatch: input $%d, output @p%d (sql=%q)",
					i, inputNums[i], outputNums[i], sql)
			}
		}
	})
}

// extractPlaceholderNumbers extracts $N values in order (outside strings/comments).
func extractPlaceholderNumbers(sql string) []int {
	var nums []int
	inStr := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if !inStr && !inBlockComment && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
		}
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if !inStr && !inBlockComment && c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if inBlockComment && c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			inBlockComment = false
			i++
			continue
		}
		if inBlockComment {
			continue
		}
		if c == '\'' {
			if inStr && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
			n := 0
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				n = n*10 + int(sql[j]-'0')
				j++
			}
			nums = append(nums, n)
			i = j - 1
		}
	}
	return nums
}

// extractAtPNNumbers extracts @pN values in order (outside strings/comments).
func extractAtPNNumbers(sql string) []int {
	var nums []int
	inStr := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if !inStr && !inBlockComment && c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
		}
		if inLineComment {
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if !inStr && !inBlockComment && c == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			inBlockComment = true
			i++
			continue
		}
		if inBlockComment && c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			inBlockComment = false
			i++
			continue
		}
		if inBlockComment {
			continue
		}
		if c == '\'' {
			if inStr && i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				continue
			}
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '@' && i+2 < len(sql) && sql[i+1] == 'p' && sql[i+2] >= '0' && sql[i+2] <= '9' {
			n := 0
			j := i + 2
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				n = n*10 + int(sql[j]-'0')
				j++
			}
			nums = append(nums, n)
			i = j - 1
		}
	}
	return nums
}
