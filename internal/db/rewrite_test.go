package db

import (
	"reflect"
	"testing"
)

// TestRewritePlaceholders_Postgres checks that Postgres is a passthrough. SQL
// must come out byte-for-byte identical so we don't regress the hot path.
func TestRewritePlaceholders_Postgres(t *testing.T) {
	cases := []string{
		"",
		"SELECT 1",
		"SELECT $1, $2, $10 FROM t WHERE x = $3",
		"INSERT INTO t (a) VALUES ('$1 literal') -- $2 in comment",
	}
	for _, q := range cases {
		if got, _ := rewritePlaceholders(q, "postgres", nil); got != q {
			t.Errorf("postgres should be passthrough; got %q for input %q", got, q)
		}
	}
}

// TestRewritePlaceholders_MySQL checks that every $N becomes a positional ?,
// and that args are reordered to match the $N ordering in the SQL text.
func TestRewritePlaceholders_MySQL(t *testing.T) {
	cases := []struct {
		in       string
		args     []any
		wantSQL  string
		wantArgs []any
	}{
		{"", nil, "", nil},
		{"SELECT 1", nil, "SELECT 1", nil},
		{"SELECT $1", []any{42}, "SELECT ?", []any{42}},
		{"SELECT $1, $2, $3", []any{"a", "b", "c"}, "SELECT ?, ?, ?", []any{"a", "b", "c"}},
		{"SELECT $10, $100", []any{0, 0, 0, 0, 0, 0, 0, 0, 0, "x", 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, "y"}, "SELECT ?, ?", []any{"x", "y"}},
		{"$1$2", []any{"a", "b"}, "??", []any{"a", "b"}},
		{"WHERE x = $1 AND y = $2", []any{1, 2}, "WHERE x = ? AND y = ?", []any{1, 2}},
		// Out-of-order $N: args should be reordered to match positional ? order
		{"SET name = $2 WHERE id = $1", []any{42, "hello"}, "SET name = ? WHERE id = ?", []any{"hello", 42}},
		{"INSERT INTO t VALUES ($1, $2, $3) RETURNING id", []any{"a", "b", "c"}, "INSERT INTO t VALUES (?, ?, ?) RETURNING id", []any{"a", "b", "c"}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			sql, args := rewritePlaceholders(c.in, "mysql", c.args)
			if sql != c.wantSQL {
				t.Errorf("mysql rewrite SQL: got %q, want %q", sql, c.wantSQL)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("mysql rewrite args: got %v, want %v", args, c.wantArgs)
			}
		})
	}
}

// TestRewritePlaceholders_MSSQL - $N maps to @pN, args pass through unchanged.
func TestRewritePlaceholders_MSSQL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT $1", "SELECT @p1"},
		{"SELECT $1, $2, $3", "SELECT @p1, @p2, @p3"},
		{"SELECT $10, $100", "SELECT @p10, @p100"},
		{"WHERE x = $1 AND y = $2", "WHERE x = @p1 AND y = @p2"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, _ := rewritePlaceholders(c.in, "mssql", nil); got != c.want {
				t.Errorf("mssql rewrite: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewritePlaceholders_SkipsStringLiterals checks that `$1` inside a
// single-quoted string stays literal. The SQL doubled-single-quote escape pair
// must NOT flip us out of string mode mid-escape.
func TestRewritePlaceholders_SkipsStringLiterals(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT '$1 is a price'", "SELECT '$1 is a price'"},
		{"SELECT '$1', $2", "SELECT '$1', ?"},                                         // outside literal
		{"SELECT 'it''s $1 ok' FROM t", "SELECT 'it''s $1 ok' FROM t"},                // escaped quote keeps us in string
		{"WHERE name = 'O''Brien' AND id = $1", "WHERE name = 'O''Brien' AND id = ?"}, // mix
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, _ := rewritePlaceholders(c.in, "mysql", nil); got != c.want {
				t.Errorf("mysql skip-string: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewritePlaceholders_NonPlaceholderDollars checks that `$$body$$`
// (Postgres dollar-quote) and `$letter` (not a placeholder) pass through
// untouched.
func TestRewritePlaceholders_NonPlaceholderDollars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT $$body$$", "SELECT $$body$$"},
		{"$abc", "$abc"},
		{"$", "$"},
		{"$ ", "$ "},
		{"SELECT $1 $2 $", "SELECT ? ? $"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, _ := rewritePlaceholders(c.in, "mysql", nil); got != c.want {
				t.Errorf("mysql non-placeholder dollar: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewritePlaceholders_UnknownEngine checks that an unknown engine passes
// through untouched rather than being silently rewritten incorrectly. The
// Connect path validates the engine upstream, so this case is defensive.
func TestRewritePlaceholders_UnknownEngine(t *testing.T) {
	in := "SELECT $1, $2"
	if got, _ := rewritePlaceholders(in, "oracle", nil); got != in {
		t.Errorf("unknown engine should be passthrough; got %q", got)
	}
}

// TestRewritePlaceholders_SkipsLineComments checks that `$N` inside
// `-- to end of line` stays literal even though it's outside any string.
// Generated SQL rarely carries a comment, but a forgotten `-- $1` would
// otherwise silently mutate to `-- ?` on MySQL.
func TestRewritePlaceholders_SkipsLineComments(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT $1 -- not a $2 placeholder", "SELECT ? -- not a $2 placeholder"},
		{"-- $1\nSELECT $1", "-- $1\nSELECT ?"},
		{"-- trailing $1 with no newline", "-- trailing $1 with no newline"},
		{"SELECT 1 -- comment\nWHERE x=$1", "SELECT 1 -- comment\nWHERE x=?"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, _ := rewritePlaceholders(c.in, "mysql", nil); got != c.want {
				t.Errorf("line-comment skip: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewritePlaceholders_SkipsBlockComments checks that `$N` inside
// `/* ... */` is preserved. We treat the first `*/` as the close (MySQL
// semantics, no nesting). Unterminated blocks are copied through to
// end-of-input.
func TestRewritePlaceholders_SkipsBlockComments(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT /* $1 */ $2", "SELECT /* $1 */ @p2"},
		{"/* leading $1 */ SELECT $2", "/* leading $1 */ SELECT @p2"},
		{"SELECT /* multi\n  line $1 \n*/ FROM t WHERE x=$3", "SELECT /* multi\n  line $1 \n*/ FROM t WHERE x=@p3"},
		{"SELECT /* unterminated $1", "SELECT /* unterminated $1"}, // no close -> whole rest is comment
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, _ := rewritePlaceholders(c.in, "mssql", nil); got != c.want {
				t.Errorf("block-comment skip: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewritePlaceholders_CommentInStringStaysLiteral checks that a `--` or
// `/*` appearing INSIDE a single-quoted string is not a comment. It stays as
// regular string content and the placeholder logic inside the string is
// already suppressed by the inStr flag.
func TestRewritePlaceholders_CommentInStringStaysLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT '-- not a $1 comment', $2", "SELECT '-- not a $1 comment', ?"},
		{"SELECT '/* still string $1 */', $2", "SELECT '/* still string $1 */', ?"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got, _ := rewritePlaceholders(c.in, "mysql", nil); got != c.want {
				t.Errorf("comment-token-in-string: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRewritePlaceholders_MySQLArgReordering tests that args are correctly
// reordered when $N placeholders are NOT in sequential order in the SQL.
func TestRewritePlaceholders_MySQLArgReordering(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		args     []any
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "out of order: WHERE before SET",
			sql:      `SET name=$2, url=$3, enabled=$7 WHERE id=$1`,
			args:     []any{99, "renamed", "https://y.com", nil, nil, nil, true},
			wantSQL:  `SET name=?, url=?, enabled=? WHERE id=?`,
			wantArgs: []any{"renamed", "https://y.com", true, 99},
		},
		{
			name:     "in order: sequential",
			sql:      `INSERT INTO t VALUES ($1, $2, $3)`,
			args:     []any{"a", "b", "c"},
			wantSQL:  `INSERT INTO t VALUES (?, ?, ?)`,
			wantArgs: []any{"a", "b", "c"},
		},
		{
			name:     "reversed: $3, $2, $1",
			sql:      `SELECT $3, $2, $1`,
			args:     []any{1, 2, 3},
			wantSQL:  `SELECT ?, ?, ?`,
			wantArgs: []any{3, 2, 1},
		},
		{
			name:     "single param, out of position",
			sql:      `UPDATE t SET name=$2 WHERE id=$1`,
			args:     []any{42, "hello"},
			wantSQL:  `UPDATE t SET name=? WHERE id=?`,
			wantArgs: []any{"hello", 42},
		},
		{
			name:     "large numbers out of order",
			sql:      `SELECT $10, $1`,
			args:     []any{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
			wantSQL:  `SELECT ?, ?`,
			wantArgs: []any{10, 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args := rewritePlaceholders(c.sql, "mysql", c.args)
			if sql != c.wantSQL {
				t.Errorf("SQL: got %q, want %q", sql, c.wantSQL)
			}
			if !reflect.DeepEqual(args, c.wantArgs) {
				t.Errorf("args: got %v, want %v", args, c.wantArgs)
			}
		})
	}
}
