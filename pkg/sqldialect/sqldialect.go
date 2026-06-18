// Package sqldialect is a stdlib-only leaf package providing dialect-aware
// SQL query-building helpers keyed by dialect name ("postgres", "mysql",
// "mssql"). Exists so pkg/core can offer Upsert, InsertDoNothing, etc.
// without importing internal/db/dialect (which re-exports these as aliases).
package sqldialect

import (
	"fmt"
	"strings"
)

// Dialect name constants passed to every builder function.
const (
	DialectPostgres = "postgres"
	DialectMySQL    = "mysql"
	DialectMSSQL    = "mssql"
)

// UpsertConfig

// UpsertConfig describes an INSERT ... ON CONFLICT / ON DUPLICATE KEY / MERGE
// statement. The caller provides the table, columns, conflict target, and
// which columns to update on conflict. The helper generates the complete
// SQL statement with $N placeholders.
//
// When DoNothing is true, the conflict path is a no-op (PG: ON CONFLICT DO
// NOTHING, MySQL: INSERT ... SELECT guarded by NOT EXISTS, MSSQL: MERGE ...
// WHEN NOT MATCHED ONLY). All three report the row count of an insert that
// did not happen as zero, so a caller can tell a duplicate from a fresh row.
//
// When UpdateCols is empty and DoNothing is false, all InsertCols (minus
// conflict columns) are updated on conflict.
type UpsertConfig struct {
	Table      string   // e.g. "items"
	InsertCols []string // all insert column names
	ConflictOn []string // unique constraint columns (e.g. ["code"] or ["tenant_id","date"])
	UpdateCols []string // columns to SET on conflict (empty = default to InsertCols minus ConflictOn)
	DoNothing  bool     // if true, skip update on conflict
}

// Upsert

// Upsert builds a dialect-appropriate INSERT ... ON CONFLICT / ON DUPLICATE KEY
// UPDATE / MERGE statement with $N placeholders for len(InsertCols) arguments.
//
//   - PG:     INSERT INTO t (...) VALUES (...) ON CONFLICT (...) DO UPDATE SET ...
//   - MySQL:  INSERT INTO t (...) VALUES (...) ON DUPLICATE KEY UPDATE ...
//   - MSSQL:  MERGE t USING (VALUES (...)) AS s(...) ON ... WHEN MATCHED ... WHEN NOT MATCHED ...
//
// DO NOTHING variants:
//   - PG:     INSERT INTO t (...) VALUES (...) ON CONFLICT (...) DO NOTHING
//   - MySQL:  INSERT INTO t (...) SELECT ... FROM DUAL WHERE NOT EXISTS (...)
//   - MSSQL:  MERGE ... WHEN NOT MATCHED THEN INSERT ... (no WHEN MATCHED)
func Upsert(dialectName string, cfg UpsertConfig) string {
	table := cfg.Table
	insertCols := cfg.InsertCols
	conflictOn := cfg.ConflictOn
	updateCols := cfg.UpdateCols
	doNothing := cfg.DoNothing

	quotedCols := make([]string, len(insertCols))
	placeholders := make([]string, len(insertCols))
	for i, c := range insertCols {
		quotedCols[i] = c
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	colsSQL := strings.Join(quotedCols, ", ")
	valsSQL := strings.Join(placeholders, ", ")

	conflictSet := make(map[string]bool, len(conflictOn))
	for _, c := range conflictOn {
		conflictSet[c] = true
	}

	effective := updateCols
	if len(effective) == 0 && !doNothing {
		for _, c := range insertCols {
			if !conflictSet[c] {
				effective = append(effective, c)
			}
		}
	}

	colPlaceholder := func(col string) string {
		if i := colIndex(insertCols, col); i >= 0 {
			return fmt.Sprintf("$%d", i+1)
		}
		return "?" // fallback, shouldn't happen
	}

	switch dialectName {
	case DialectMySQL:
		return mysqlUpsert(table, colsSQL, valsSQL, conflictOn, insertCols, effective, doNothing, colPlaceholder)
	case DialectMSSQL:
		return mssqlUpsert(table, insertCols, quotedCols, conflictOn, effective, doNothing)
	default: // postgres
		return pgUpsert(table, colsSQL, valsSQL, conflictOn, effective, doNothing)
	}
}

func pgUpsert(table, colsSQL, valsSQL string, conflictOn, updateCols []string, doNothing bool) string {
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, colsSQL, valsSQL)
	if doNothing {
		return stmt + " ON CONFLICT (" + strings.Join(conflictOn, ", ") + ") DO NOTHING"
	}
	var sets []string
	for _, c := range updateCols {
		sets = append(sets, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
	}
	return stmt + " ON CONFLICT (" + strings.Join(conflictOn, ", ") + ") DO UPDATE SET " + strings.Join(sets, ", ")
}

func mysqlUpsert(table, colsSQL, valsSQL string, conflictOn, insertCols, updateCols []string, doNothing bool, colPH func(string) string) string {
	if doNothing {
		// A guarded INSERT ... SELECT, and neither INSERT IGNORE nor a
		// self-assigning ON DUPLICATE KEY UPDATE.
		//
		// IGNORE downgrades every error the statement can raise to a warning:
		// a foreign key with no parent, a value too long for its column, a
		// NULL in a NOT NULL column. The caller then reads zero affected rows
		// as "it was already there" and answers 409 to what was really a
		// rejected write, or worse, treats lost data as a duplicate.
		//
		// ON DUPLICATE KEY UPDATE c = c narrows that back to swallowing a
		// duplicate key and nothing else, but the engine asks MySQL to count
		// the rows a statement matched rather than the rows it changed, so
		// that an idempotent UPDATE does not look like an update against a
		// missing row. Under that setting an untouched duplicate reports one
		// row, which is exactly what a fresh insert reports, and the count
		// stops telling the two apart.
		//
		// INSERT ... SELECT counts rows actually inserted, so it reads the
		// same either way. The probe and the insert are not one step, so a
		// concurrent writer can still take the key in between. The duplicate
		// key MySQL then raises says the row exists, which is the answer the
		// probe would have given a moment later.
		if probe, ok := mysqlKeyProbe(table, conflictOn, insertCols); ok {
			return fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM DUAL WHERE NOT EXISTS (%s)",
				table, colsSQL, valsSQL, probe)
		}
		// A conflict column the insert does not bind has no value to probe
		// for. Swallow the duplicate without reporting it rather than emit a
		// statement that cannot run.
		return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
			table, colsSQL, valsSQL, noopAssignment(conflictOn, insertCols))
	}
	var sets []string
	for _, c := range updateCols {
		sets = append(sets, fmt.Sprintf("%s = %s", c, colPH(c)))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		table, colsSQL, valsSQL, strings.Join(sets, ", "))
}

// mysqlKeyProbe builds the subquery that asks whether the conflict key is
// already stored. It compares against the same $N placeholders the insert
// binds, so guarding the insert costs no extra arguments: the engine's
// placeholder rewriter repeats an argument wherever its $N appears.
//
// It reports false when the conflict key is not covered by the inserted
// columns, since there is then no bound value to compare against.
func mysqlKeyProbe(table string, conflictOn, insertCols []string) (string, bool) {
	if len(conflictOn) == 0 {
		return "", false
	}
	conds := make([]string, 0, len(conflictOn))
	for _, c := range conflictOn {
		idx := colIndex(insertCols, c)
		if idx < 0 {
			return "", false
		}
		conds = append(conds, fmt.Sprintf("existing.%s = $%d", c, idx+1))
	}
	return fmt.Sprintf("SELECT 1 FROM %s AS existing WHERE %s", table, strings.Join(conds, " AND ")), true
}

// colIndex returns the position of col in cols, or -1.
func colIndex(cols []string, col string) int {
	for i, c := range cols {
		if c == col {
			return i
		}
	}
	return -1
}

// noopAssignment returns an ON DUPLICATE KEY UPDATE assignment that changes
// nothing, so the clause only swallows the duplicate. It prefers a conflict
// column because one is guaranteed to be part of the key that matched.
func noopAssignment(conflictOn, insertCols []string) string {
	col := ""
	switch {
	case len(conflictOn) > 0:
		col = conflictOn[0]
	case len(insertCols) > 0:
		col = insertCols[0]
	default:
		return "id = id"
	}
	return col + " = " + col
}

func mssqlUpsert(table string, insertCols, quotedCols, conflictOn, updateCols []string, doNothing bool) string {
	vals := make([]string, len(insertCols))
	for i := range insertCols {
		vals[i] = fmt.Sprintf("$%d", i+1)
	}
	valsSQL := strings.Join(vals, ", ")
	colsSQL := strings.Join(quotedCols, ", ")

	srcAliases := make([]string, len(conflictOn))
	for i, c := range conflictOn {
		srcAliases[i] = fmt.Sprintf("s.%s = t.%s", c, c)
	}
	onCond := strings.Join(srcAliases, " AND ")

	// HOLDLOCK is required, not an optimization. Without it MERGE takes only a
	// short update lock while it evaluates the ON clause and releases it before
	// inserting, so two sessions upserting the same key both take the NOT
	// MATCHED path and the second one violates the unique index. HOLDLOCK holds
	// a range lock on the target for the whole statement, which is the
	// serialization MERGE-as-upsert needs. PG and MySQL get this from ON
	// CONFLICT / ON DUPLICATE KEY for free.
	stmt := fmt.Sprintf("MERGE %s WITH (HOLDLOCK) AS t USING (VALUES (%s)) AS s(%s) ON %s",
		table, valsSQL, colsSQL, onCond)

	if doNothing {
		// No WHEN MATCHED clause: insert only.
		return stmt + fmt.Sprintf(" WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s);",
			colsSQL, mssqlSrcCols(insertCols))
	}

	var setParts []string
	for _, c := range updateCols {
		setParts = append(setParts, fmt.Sprintf("%s = s.%s", c, c))
	}

	return stmt +
		fmt.Sprintf(" WHEN MATCHED THEN UPDATE SET %s", strings.Join(setParts, ", ")) +
		fmt.Sprintf(" WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s);",
			colsSQL, mssqlSrcCols(insertCols))
}

// mssqlSrcCols builds "s.id, s.code, ..." for the MSSQL MERGE VALUES clause.
func mssqlSrcCols(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "s." + c
	}
	return strings.Join(parts, ", ")
}

// InsertDoNothing

// InsertDoNothing builds a dialect-appropriate INSERT that silently ignores
// duplicate key violations. $N placeholders are generated for len(cols) args.
//
//   - PG:     INSERT INTO t (...) VALUES (...) ON CONFLICT (conflictOn) DO NOTHING
//   - MySQL:  INSERT INTO t (...) SELECT ... FROM DUAL WHERE NOT EXISTS (...)
//   - MSSQL:  MERGE ... WHEN NOT MATCHED THEN INSERT (no WHEN MATCHED)
//
// A duplicate reports zero rows affected and a fresh row reports one, on every
// dialect. Prefer the executing helper in pkg/core over reading that count by
// hand: on MySQL the probe and the insert are separate steps, so a caller that
// loses the race to a concurrent writer gets a duplicate-key error instead of
// a zero count, and the helper folds the two onto one answer.
func InsertDoNothing(dialectName string, table string, cols []string, conflictOn []string) string {
	return Upsert(dialectName, UpsertConfig{
		Table:      table,
		InsertCols: cols,
		ConflictOn: conflictOn,
		DoNothing:  true,
	})
}

// ILike

// ILike returns a SQL fragment for case-insensitive LIKE matching.
// placeholder is the caller's bind-parameter reference (e.g. "$1", "?").
//
//   - PG:     column ILIKE '%' || $N || '%'
//   - MySQL:  column LIKE CONCAT('%', $N, '%')
//   - MSSQL:  CHARINDEX($N, column) > 0
func ILike(dialectName, column, placeholder string) string {
	switch dialectName {
	case DialectMySQL:
		return fmt.Sprintf("%s LIKE CONCAT('%%', %s, '%%')", column, placeholder)
	case DialectMSSQL:
		// CHARINDEX rather than LIKE. SQL Server caps a LIKE pattern at 8000
		// bytes, which is 4000 characters of NVARCHAR, and wrapping the value
		// in % takes it two over, so a search for a 4000-character value would
		// fail with "String or binary data would be truncated". CHARINDEX
		// takes the value as an operand rather than compiling it into a
		// pattern, so the cap does not apply.
		//
		// Neither form is index-seekable here, since the match is unanchored
		// either way, and both follow the column's collation for case
		// sensitivity, so this changes nothing but the ceiling.
		return fmt.Sprintf("CHARINDEX(%s, %s) > 0", placeholder, column)
	default: // postgres
		return fmt.Sprintf("%s ILIKE '%%' || %s || '%%'", column, placeholder)
	}
}

// ILikeEscapeClause returns the ESCAPE clause that belongs after an ILike
// expression, including its leading space, or "" where none does.
//
// ILike does not emit a LIKE on MSSQL: it emits CHARINDEX, which takes the
// value as an operand rather than compiling it into a pattern. ESCAPE is only
// valid after a LIKE predicate, so appending one there is a syntax error, and
// a caller that hard-codes the clause silently breaks the statement on that
// engine alone. Ask for the clause instead of writing it.
func ILikeEscapeClause(dialectName string) string {
	switch dialectName {
	case DialectMySQL:
		return ` ESCAPE '\\'`
	case DialectMSSQL:
		return ""
	default:
		return ` ESCAPE '\'`
	}
}

// ILikeValue prepares a value to be bound to an ILike expression.
//
// On the LIKE dialects the wildcards have to be escaped or a value containing
// % or _ matches far more than the caller asked for. On MSSQL the value is an
// operand, so escaping it would search for the escape characters themselves:
// erasing "a_b@example.com" would look for a literal "a\_b@example.com" and
// match nothing.
func ILikeValue(dialectName, value string) string {
	if dialectName == DialectMSSQL {
		return value
	}
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(value)
}

// Cast

// Cast returns a SQL fragment that casts expr to targetType.
//
//   - PG:     expr::targetType  (shorthand) or CAST(expr AS targetType)
//   - MySQL:  CAST(expr AS targetType)
//   - MSSQL:  CAST(expr AS targetType)
//
// pgShorthand (default true) selects PG :: syntax. Pass false to force
// CAST(expr AS ...) on all dialects.
func Cast(dialectName, expr, targetType string, pgShorthand ...bool) string {
	useShorthand := true
	if len(pgShorthand) > 0 {
		useShorthand = pgShorthand[0]
	}
	if dialectName == DialectPostgres && useShorthand {
		return expr + "::" + targetType
	}
	return "CAST(" + expr + " AS " + targetType + ")"
}

// LimitOffset

// LimitOffset returns the dialect-appropriate pagination clause using literal
// values. For parameterized queries, use LimitOffsetPlaceholders instead.
//
//   - PG/MySQL: LIMIT limit OFFSET offset
//   - MSSQL:    OFFSET offset ROWS FETCH NEXT limit ROWS ONLY
func LimitOffset(dialectName string, limit, offset int) string {
	if dialectName == DialectMSSQL {
		// MSSQL rejects FETCH NEXT 0 ROWS ONLY. limit<=0 means "no rows", so
		// skip past every possible row with a max-offset clause instead.
		if limit <= 0 {
			return "OFFSET 9223372036854775807 ROWS FETCH NEXT 1 ROW ONLY"
		}
		rowWord := "ROWS"
		if limit == 1 {
			rowWord = "ROW"
		}
		return fmt.Sprintf("OFFSET %d ROWS FETCH NEXT %d %s ONLY", offset, limit, rowWord)
	}
	return fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
}

// LimitOffsetPlaceholders

// LimitOffsetPlaceholders returns the pagination clause using $N placeholders.
// limitIdx and offsetIdx are 1-based indices (e.g. 3, 4 -> $3, $4).
//
//   - PG/MySQL: LIMIT $limitIdx OFFSET $offsetIdx
//   - MSSQL:    OFFSET $offsetIdx ROWS FETCH NEXT $limitIdx ROWS ONLY
//
// A limit of 0 means "no rows" under this engine's pagination contract, and
// Postgres and MySQL honor it directly. SQL Server rejects FETCH NEXT 0 ROWS
// ONLY at execution time, so a caller who bound 0 would get a 503 from a
// request that is valid everywhere else. The literal builder above already skips past every
// row instead. The same has to hold when the value arrives as a parameter,
// where it cannot be inspected while the clause is built. OFFSET and FETCH
// accept an expression over parameters, so the choice is made by the engine at
// execution time rather than here.
func LimitOffsetPlaceholders(dialectName string, limitIdx, offsetIdx int) string {
	if dialectName == DialectMSSQL {
		// The CAST is required, not decoration: without it SQL Server rejects
		// the CASE with "The number of rows provided for a OFFSET clause must
		// be an integer".
		return fmt.Sprintf(
			"OFFSET CAST(CASE WHEN $%d <= 0 THEN 9223372036854775807 ELSE $%d END AS BIGINT) ROWS "+
				"FETCH NEXT CAST(CASE WHEN $%d <= 0 THEN 1 ELSE $%d END AS BIGINT) ROWS ONLY",
			limitIdx, offsetIdx, limitIdx, limitIdx,
		)
	}
	return fmt.Sprintf("LIMIT $%d OFFSET $%d", limitIdx, offsetIdx)
}

// QuoteIdentifier

// QuoteIdentifier wraps a SQL identifier in dialect-appropriate quoting
// (PG: "...", MySQL: `...`, MSSQL: [...]). Embedded close-quote characters are
// doubled. Callers must validate the identifier before quoting it.
func QuoteIdentifier(dialectName, name string) string {
	switch dialectName {
	case DialectMySQL:
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	case DialectMSSQL:
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	default: // postgres
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

// Syntax is the part of a database engine's SQL surface that any caller may
// need: what the engine is called, how it spells a bind parameter, how it
// names the current timestamp, and how it quotes an identifier.
//
// It is deliberately small. Generating DDL needs a much wider surface, with
// per-engine column types and ALTER forms, and nothing outside the schema
// engine calls any of that. Keeping the two apart means a caller that only
// builds queries does not depend on the DDL layer.
//
// Callers must still validate an identifier before quoting it. Quoting is
// defense in depth, not a substitute for validation.
type Syntax interface {
	// Name returns the canonical driver name: "postgres", "mysql" or "mssql".
	Name() string

	// Placeholder returns the positional bind parameter for the n-th argument,
	// counting from 1. PostgreSQL uses $1, MySQL and SQL Server use ?.
	Placeholder(n int) string

	// NowFunc returns the SQL expression for the current timestamp.
	NowFunc() string

	// QuoteIdentifier wraps a SQL identifier in the engine's quoting style,
	// doubling any embedded quote character.
	QuoteIdentifier(name string) string
}
