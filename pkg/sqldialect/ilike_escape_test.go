package sqldialect_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// ESCAPE is valid only after LIKE, and ILike emits CHARINDEX on MSSQL, so a
// caller that appends a hard-coded ESCAPE clause breaks the statement on that
// engine alone. ILikeEscapeClause gives the clause each engine takes.
func TestILikeEscapeClause_MSSQLTakesNone(t *testing.T) {
	assert.Equal(t, "", sqldialect.ILikeEscapeClause(sqldialect.DialectMSSQL),
		"CHARINDEX takes no pattern, so it takes no ESCAPE")
	assert.Equal(t, ` ESCAPE '\'`, sqldialect.ILikeEscapeClause("postgres"))
	assert.Equal(t, ` ESCAPE '\\'`, sqldialect.ILikeEscapeClause(sqldialect.DialectMySQL))
}

// The expression and its clause have to agree: whatever ILike emits, appending
// ILikeEscapeClause to it must stay valid SQL for that dialect.
func TestILikeEscapeClause_MatchesWhatILikeEmits(t *testing.T) {
	for _, d := range []string{"postgres", sqldialect.DialectMySQL, sqldialect.DialectMSSQL} {
		expr := sqldialect.ILike(d, "col", "$1") + sqldialect.ILikeEscapeClause(d)
		if d == sqldialect.DialectMSSQL {
			assert.NotContains(t, expr, "ESCAPE", "%s: CHARINDEX must not carry an ESCAPE", d)
			assert.Contains(t, expr, "CHARINDEX", d)
		} else {
			assert.Contains(t, expr, "ESCAPE", "%s: a LIKE pattern needs its escape character named", d)
			assert.Contains(t, expr, "LIKE", d)
		}
	}
}

func TestILikeValue_EscapesOnlyWhereItIsAPattern(t *testing.T) {
	const raw = `a_b%c\d`
	assert.Equal(t, raw, sqldialect.ILikeValue(sqldialect.DialectMSSQL, raw),
		"an operand must reach the engine as the caller wrote it")
	assert.Equal(t, `a\_b\%c\\d`, sqldialect.ILikeValue("postgres", raw))
	assert.Equal(t, `a\_b\%c\\d`, sqldialect.ILikeValue(sqldialect.DialectMySQL, raw))
}

// A value with no wildcards is untouched everywhere, so the common case cannot
// drift between dialects.
func TestILikeValue_PlainValueIsUnchanged(t *testing.T) {
	const email = "user@example.com"
	for _, d := range []string{"postgres", sqldialect.DialectMySQL, sqldialect.DialectMSSQL} {
		assert.Equal(t, email, sqldialect.ILikeValue(d, email), d)
	}
}
