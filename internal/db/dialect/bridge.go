// bridge.go provides reusable dialect-aware SQL query-building helpers.
//
// These helpers delegate to pkg/sqldialect: the canonical home for the pure
// SQL-string builders. This internal package keeps the Dialect-interface-based
// wrappers so that internal callers that already hold a Dialect value don't
// need to extract its name before every call.
//
// All helpers assume the Querier rewrites $N placeholders to the engine-native
// form (-> ? for MySQL, -> @pN for MSSQL).
//
// Identifiers are NOT quoted: callers use hardcoded safe names. For dynamic
// identifiers from user input, use Dialect.QuoteIdentifier explicitly.

package dialect

import (
	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// UpsertConfig is an alias to sqldialect.UpsertConfig so that plugin stores
// receive the same concrete type whether they import internal/db/dialect or
// pkg/core (via sqldialect).
type UpsertConfig = sqldialect.UpsertConfig

// Upsert returns a dialect-appropriate INSERT ... upsert statement with $N
// placeholders for len(InsertCols) arguments. Wraps sqldialect.Upsert.
func Upsert(d Dialect, cfg UpsertConfig) string {
	return sqldialect.Upsert(d.Name(), cfg)
}

// InsertDoNothing returns a dialect-appropriate INSERT that silently ignores
// duplicate key violations. Wraps sqldialect.InsertDoNothing.
func InsertDoNothing(d Dialect, table string, cols []string, conflictOn []string) string {
	return sqldialect.InsertDoNothing(d.Name(), table, cols, conflictOn)
}

// ILike returns a SQL fragment for case-insensitive LIKE matching.
// Wraps sqldialect.ILike.
func ILike(d Dialect, column, placeholder string) string {
	return sqldialect.ILike(d.Name(), column, placeholder)
}

// Cast returns a SQL fragment that casts expr to the given SQL type.
// Wraps sqldialect.Cast.
func Cast(d Dialect, expr, targetType string, pgShorthand ...bool) string {
	return sqldialect.Cast(d.Name(), expr, targetType, pgShorthand...)
}

// LimitOffset returns the dialect-appropriate pagination clause using
// literal values (no placeholders). Wraps sqldialect.LimitOffset.
func LimitOffset(d Dialect, limit, offset int) string {
	return sqldialect.LimitOffset(d.Name(), limit, offset)
}

// LimitOffsetPlaceholders returns the dialect-appropriate pagination clause
// using $N placeholders instead of literal values. Wraps sqldialect.LimitOffsetPlaceholders.
func LimitOffsetPlaceholders(d Dialect, limitIdx, offsetIdx int) string {
	return sqldialect.LimitOffsetPlaceholders(d.Name(), limitIdx, offsetIdx)
}
