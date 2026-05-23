// dialect_builders.go exposes dialect-aware SQL query-building helpers for
// plugin stores. Plugins import core (not internal/db/dialect or
// pkg/sqldialect directly), so these thin wrappers delegate to pkg/sqldialect.
//
// InsertIfAbsent is the one helper here that runs the statement as well as
// building it, because reading its result correctly takes engine knowledge a
// caller should not have to carry.
//
// All helpers assume the Querier rewrites $N placeholders to the engine-
// native form (? for MySQL, @pN for MSSQL).

package core

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"

	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
	"github.com/lyeve-labs/lyeve-core/pkg/sqlx"
)

// UpsertConfig is a type alias to sqldialect.UpsertConfig so plugins
// receive the same concrete type as internal/db/dialect callers.
// See sqldialect.UpsertConfig for field documentation.
type UpsertConfig = sqldialect.UpsertConfig

// Upsert returns a dialect-appropriate INSERT ... upsert statement for the
// given dialect name ("postgres", "mysql", "mssql").
//
// Usage:
//
//	query := core.Upsert(host.Dialect(), core.UpsertConfig{
//	    Table:      "sys_error_codes",
//	    InsertCols: []string{"id", "code", "title", "severity", "created_at", "updated_at"},
//	    ConflictOn: []string{"code"},
//	})
//	_, err := host.Querier(ctx).Exec(ctx, query, id, code, title, severity, createdAt, updatedAt)
func Upsert(dialectName string, cfg UpsertConfig) string {
	return sqldialect.Upsert(dialectName, cfg)
}

// InsertDoNothing returns a dialect-appropriate INSERT that silently ignores
// duplicate key violations. conflictOn lists the unique constraint columns.
//
// A duplicate reports zero rows affected and a fresh row reports one. Prefer
// InsertIfAbsent over reading that count directly: on MySQL a caller that
// loses the race to a concurrent writer gets a duplicate-key error rather than
// a zero count, and the helper folds both onto one answer.
func InsertDoNothing(dialectName string, table string, cols []string, conflictOn []string) string {
	return sqldialect.InsertDoNothing(dialectName, table, cols, conflictOn)
}

// InsertIfAbsent inserts a row unless one with the same conflict key is
// already stored, and reports whether this call is the one that created it.
// args bind the $N placeholders for cols, in order.
//
// Use it wherever a store answers 409 for a key that is taken:
//
//	created, err := core.InsertIfAbsent(ctx, s.host.Querier(ctx), s.host.Dialect(),
//	    "sys_widgets", cols, []string{"tenant_id", "name"}, args...)
//	if err != nil {
//	    return fmt.Errorf("create widget: %w", err)
//	}
//	if !created {
//	    return errAlreadyExists
//	}
//
// The three engines each report an insert that did not happen differently, and
// on MySQL the row count alone cannot carry the answer: the statement probes
// for the key before inserting, so a writer that takes the key in between
// leaves this call holding a duplicate-key error instead of a zero count. Both
// mean the row is there, so both report false.
//
// Only a duplicate key reads that way. A foreign key with no parent, a value
// too long for its column and a NULL in a NOT NULL column all reach the caller
// as errors, because a write the database rejected is not a row someone else
// already wrote.
// A lost deadlock is retried rather than returned. Concurrent callers inserting
// different rows still contend on MySQL, where the conflict path takes gap
// locks and one of the pair is asked to restart the transaction. The statement
// did not commit, so running it again is not a second write: the retry either
// inserts or meets the duplicate, which is the answer the caller wanted either
// way. Returning the deadlock instead would make "is this row already there"
// fail under the concurrency it exists to arbitrate.
func InsertIfAbsent(ctx context.Context, q Querier, dialectName, table string, cols, conflictOn []string, args ...any) (bool, error) {
	var tag CommandTag
	err := sqlx.RetryOnTxConflict(ctx, "insert_if_absent", func() error {
		var execErr error
		tag, execErr = q.Exec(ctx, InsertDoNothing(dialectName, table, cols, conflictOn), args...)
		return execErr
	})
	if err != nil {
		if isDuplicateKey(err) {
			return false, nil
		}
		return false, err
	}
	return tag.RowsAffected > 0, nil
}

// IsDuplicateKey reports whether err is the engine saying a unique key is
// already taken, across all three supported dialects.
//
// Plugins need this to tell "that name is in use" apart from "the database is
// unreachable": the first is a 409 the caller can act on, the second a 503.
// Without it the only options are importing all three drivers or matching on
// message text, and message text differs per driver version.
func IsDuplicateKey(err error) bool { return isDuplicateKey(err) }

// isDuplicateKey reports whether err is the engine saying a unique key is
// already taken.
//
// Postgres and SQL Server are matched through the methods their drivers
// expose, so this package does not depend on either. MySQL collapses every
// integrity violation onto SQLSTATE 23000, which leaves the vendor number as
// the only thing separating a duplicate key from a missing foreign key, and it
// is a struct field rather than a method.
func isDuplicateKey(err error) bool {
	var pgLike interface{ SQLState() string }
	if errors.As(err, &pgLike) {
		return pgLike.SQLState() == "23505"
	}

	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 1062 duplicate entry, 1169 duplicate key on write.
		return myErr.Number == 1062 || myErr.Number == 1169
	}

	var msLike interface{ SQLErrorNumber() int32 }
	if errors.As(err, &msLike) {
		// 2601 duplicate key in index, 2627 unique constraint violated.
		n := msLike.SQLErrorNumber()
		return n == 2601 || n == 2627
	}
	return false
}

// ILike returns a SQL fragment for case-insensitive LIKE matching.
//
//   - PG:    column ILIKE '%' || $N || '%'
//   - MySQL: column LIKE CONCAT('%', $N, '%')
//   - MSSQL: CHARINDEX($N, column) > 0
//
// placeholder is the bind-parameter reference (e.g. "$1" or "?").
//
// Pair it with ILikeEscapeClause and ILikeValue rather than hard-coding an
// ESCAPE clause or escaping the value yourself: MSSQL takes neither.
func ILike(dialectName, column, placeholder string) string {
	return sqldialect.ILike(dialectName, column, placeholder)
}

// ILikeEscapeClause returns the ESCAPE clause that belongs after an ILike
// expression, including its leading space, or "" where none does.
func ILikeEscapeClause(dialectName string) string {
	return sqldialect.ILikeEscapeClause(dialectName)
}

// ILikeValue prepares a value to be bound to an ILike expression, escaping
// wildcards only on the dialects that treat it as a pattern.
func ILikeValue(dialectName, value string) string {
	return sqldialect.ILikeValue(dialectName, value)
}

// Cast returns a SQL fragment that casts expr to the given SQL type.
//
//   - PG:    expr::targetType  (default shorthand)
//   - MySQL: CAST(expr AS targetType)
//   - MSSQL: CAST(expr AS targetType)
//
// Pass pgShorthand=false to force CAST() on all dialects.
func Cast(dialectName, expr, targetType string, pgShorthand ...bool) string {
	return sqldialect.Cast(dialectName, expr, targetType, pgShorthand...)
}

// LimitOffset returns the dialect-appropriate pagination clause.
//
//   - PG/MySQL: LIMIT limit OFFSET offset
//   - MSSQL:    OFFSET offset ROWS FETCH NEXT limit ROW(S) ONLY
func LimitOffset(dialectName string, limit, offset int) string {
	return sqldialect.LimitOffset(dialectName, limit, offset)
}

// LimitOffsetPlaceholders returns the dialect-appropriate pagination clause
// using $N placeholders instead of literal values. limitIdx and offsetIdx
// are the 1-based placeholder indices (e.g. $3, $4). The engine's rewrite()
// translates $N->?/@pN at execution time.
//
//   - PG/MySQL: LIMIT $limitIdx OFFSET $offsetIdx
//   - MSSQL:    OFFSET $offsetIdx ROWS FETCH NEXT $limitIdx ROWS ONLY
func LimitOffsetPlaceholders(dialectName string, limitIdx, offsetIdx int) string {
	return sqldialect.LimitOffsetPlaceholders(dialectName, limitIdx, offsetIdx)
}

// QuoteIdentifier wraps a SQL identifier in dialect-appropriate quoting:
//
//   - PG:     "identifier"
//   - MySQL:  `identifier`
//   - MSSQL:  [identifier]
//
// Embed any close-quote characters are doubled per dialect convention.
// Callers MUST validate identifiers via domain.ValidateIdentifier before quoting.
func QuoteIdentifier(dialectName, name string) string {
	return sqldialect.QuoteIdentifier(dialectName, name)
}
