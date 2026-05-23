// pbt_quote_test.go: Property-based tests for dialect QuoteIdentifier.
//
// Encodes cross-dialect invariants using pgregory.net/rapid:
//   - Quoting wraps identifiers (output = open + escaped + close).
//   - Embedded quote chars are always doubled (injection-safe).
//   - Output length >= input length + 2 (wrapper chars always present).
//   - After quoting, the identifier content can be safely round-tripped.
//
// Run:
//
//	go test -race -run TestPBT ./internal/db/dialect/

package dialect_test

import (
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
)

// genIdentifier generates random identifiers including adversarial inputs:
// normal names, empty strings, strings with embedded quote chars, and
// SQL injection attempts.
func genIdentifier() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		choice := rapid.IntRange(0, 6).Draw(t, "kind")
		switch choice {
		case 0: // normal alphanumeric
			return rapid.StringMatching(`[A-Za-z_][A-Za-z0-9_]*`).Draw(t, "normal")
		case 1: // empty
			return ""
		case 2: // with embedded double-quote (PG)
			prefix := rapid.StringMatching(`[a-z]+`).Draw(t, "pfx")
			suffix := rapid.StringMatching(`[a-z]+`).Draw(t, "sfx")
			return prefix + `"` + suffix
		case 3: // with embedded backtick (MySQL)
			prefix := rapid.StringMatching(`[a-z]+`).Draw(t, "pfx")
			suffix := rapid.StringMatching(`[a-z]+`).Draw(t, "sfx")
			return prefix + "`" + suffix
		case 4: // with embedded close-bracket (MSSQL)
			prefix := rapid.StringMatching(`[a-z]+`).Draw(t, "pfx")
			suffix := rapid.StringMatching(`[a-z]+`).Draw(t, "sfx")
			return prefix + "]" + suffix
		case 5: // SQL injection attempt
			return rapid.OneOf(
				rapid.Just(`x"; DROP TABLE users --`),
				rapid.Just("x`; DROP TABLE users --"),
				rapid.Just("x]; DROP TABLE users --"),
				rapid.Just(`"; SELECT 1 --`),
			).Draw(t, "injection")
		default: // arbitrary printable
			return rapid.StringMatching(`[\x20-\x7E]{0,50}`).Draw(t, "arbitrary")
		}
	})
}

// Postgres: double-quote quoting

// Property: Postgres QuoteIdentifier always produces `"..."` wrapping.
func TestPBT_Postgres_QuoteAlwaysWrapped(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.Postgres{}.QuoteIdentifier(name)

		if !strings.HasPrefix(got, `"`) {
			t.Errorf("PG: missing opening quote: got %q for input %q", got, name)
		}
		if !strings.HasSuffix(got, `"`) {
			t.Errorf("PG: missing closing quote: got %q for input %q", got, name)
		}
		// Must be at least 2 chars (the two quotes) even for empty input.
		if len(got) < 2 {
			t.Errorf("PG: output too short: len=%d for input %q", len(got), name)
		}
	})
}

// Property: Postgres QuoteIdentifier doubles all embedded double-quote chars.
func TestPBT_Postgres_EmbeddedQuotesDoubled(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.Postgres{}.QuoteIdentifier(name)

		// Inner content (without outer quotes) should have no unescaped " chars.
		inner := got[1 : len(got)-1]

		// Walk the inner content. Every " must be followed by another " (doubled).
		for i := 0; i < len(inner); i++ {
			if inner[i] == '"' {
				if i+1 >= len(inner) || inner[i+1] != '"' {
					t.Errorf("PG: unescaped \" at pos %d in inner %q (from input %q)",
						i, inner, name)
				}
				i++ // skip the pair
			}
		}
	})
}

// Property: Postgres output length = input length + 2*(number of embedded ") + 2.
func TestPBT_Postgres_OutputLengthPredictable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.Postgres{}.QuoteIdentifier(name)

		embeddedQuotes := strings.Count(name, `"`)
		wantLen := len(name) + 2 + embeddedQuotes // each " becomes ""
		if len(got) != wantLen {
			t.Errorf("PG: len=%d, want %d (input=%q, got=%q)", len(got), wantLen, name, got)
		}
	})
}

// MySQL: backtick quoting

// Property: MySQL QuoteIdentifier always produces backtick wrapping.
func TestPBT_MySQL_QuoteAlwaysWrapped(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.MySQL{}.QuoteIdentifier(name)

		if !strings.HasPrefix(got, "`") {
			t.Errorf("MySQL: missing opening backtick: got %q for input %q", got, name)
		}
		if !strings.HasSuffix(got, "`") {
			t.Errorf("MySQL: missing closing backtick: got %q for input %q", got, name)
		}
		if len(got) < 2 {
			t.Errorf("MySQL: output too short: len=%d for input %q", len(got), name)
		}
	})
}

// Property: MySQL QuoteIdentifier doubles all embedded backtick chars.
func TestPBT_MySQL_EmbeddedBackticksDoubled(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.MySQL{}.QuoteIdentifier(name)

		inner := got[1 : len(got)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '`' {
				if i+1 >= len(inner) || inner[i+1] != '`' {
					t.Errorf("MySQL: unescaped backtick at pos %d in inner %q (from input %q)",
						i, inner, name)
				}
				i++
			}
		}
	})
}

// Property: MySQL output length = input length + 2*(embedded backticks) + 2.
func TestPBT_MySQL_OutputLengthPredictable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.MySQL{}.QuoteIdentifier(name)

		embedded := strings.Count(name, "`")
		wantLen := len(name) + 2 + embedded
		if len(got) != wantLen {
			t.Errorf("MySQL: len=%d, want %d (input=%q, got=%q)", len(got), wantLen, name, got)
		}
	})
}

// MSSQL: bracket quoting

// Property: MSSQL QuoteIdentifier always produces `[...]` wrapping.
func TestPBT_MSSQL_QuoteAlwaysWrapped(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.MSSQL{}.QuoteIdentifier(name)

		if !strings.HasPrefix(got, "[") {
			t.Errorf("MSSQL: missing opening bracket: got %q for input %q", got, name)
		}
		if !strings.HasSuffix(got, "]") {
			t.Errorf("MSSQL: missing closing bracket: got %q for input %q", got, name)
		}
		if len(got) < 2 {
			t.Errorf("MSSQL: output too short: len=%d for input %q", len(got), name)
		}
	})
}

// Property: MSSQL QuoteIdentifier doubles all embedded `]` chars.
func TestPBT_MSSQL_EmbeddedBracketsDoubled(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.MSSQL{}.QuoteIdentifier(name)

		inner := got[1 : len(got)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == ']' {
				if i+1 >= len(inner) || inner[i+1] != ']' {
					t.Errorf("MSSQL: unescaped ] at pos %d in inner %q (from input %q)",
						i, inner, name)
				}
				i++
			}
		}
	})
}

// Property: MSSQL output length = input length + 2*(embedded ]) + 2.
func TestPBT_MSSQL_OutputLengthPredictable(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		got := dialect.MSSQL{}.QuoteIdentifier(name)

		embedded := strings.Count(name, "]")
		wantLen := len(name) + 2 + embedded
		if len(got) != wantLen {
			t.Errorf("MSSQL: len=%d, want %d (input=%q, got=%q)", len(got), wantLen, name, got)
		}
	})
}

// Cross-dialect injection safety

// Property: For any identifier containing SQL injection payloads, no dialect
// produces output that breaks out of the quoting boundary.
func TestPBT_AllDialects_InjectionSafe(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		dialects := []dialect.Dialect{dialect.Postgres{}, dialect.MySQL{}, dialect.MSSQL{}}
		quoteChars := []byte{'"', '`', '['}
		closeChars := []byte{'"', '`', ']'}

		for i, d := range dialects {
			got := d.QuoteIdentifier(name)

			// Must start with open quote and end with close quote.
			if got[0] != quoteChars[i] {
				t.Errorf("%s: doesn't start with %c: %q", d.Name(), quoteChars[i], got)
			}
			if got[len(got)-1] != closeChars[i] {
				t.Errorf("%s: doesn't end with %c: %q", d.Name(), closeChars[i], got)
			}

			// The inner content must not contain an unescaped close-quote that
			// would allow breaking out.
			inner := got[1 : len(got)-1]
			escaped := string(closeChars[i]) + string(closeChars[i])
			// Replace all escaped pairs, then check no lone close-quote remains.
			cleaned := strings.ReplaceAll(inner, escaped, "")
			if strings.ContainsRune(cleaned, rune(closeChars[i])) {
				t.Errorf("%s: injection possible - lone %c in inner %q (from %q, got %q)",
					d.Name(), closeChars[i], cleaned, name, got)
			}
		}
	})
}

// Property: QuoteIdentifier never panics.
func TestPBT_AllDialects_NeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := genIdentifier().Draw(t, "name")
		dialects := []dialect.Dialect{dialect.Postgres{}, dialect.MySQL{}, dialect.MSSQL{}}
		for _, d := range dialects {
			d.QuoteIdentifier(name)
		}
	})
}
