package debug

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

var _ core.DebugRecorder = (*Tracer)(nil)

// RecordQuery implements core.DebugRecorder. Records a query segment with its SQL and arguments.
func (t *Tracer) RecordQuery(name string, kind string, dur time.Duration, sql string, args []any) {
	t.Record(name, Kind(kind), dur, DBQueryDetail{
		SQL:  sql,
		Args: args,
	})
}

// Querier wrapper

// Querier wraps a core.Querier with debug recording.
// Each Query/QueryRow/Exec call is timed and recorded against the given Tracer.
// When rawDB is non-nil and dialect is supported, SELECT queries are
// followed up with an EXPLAIN to capture the query plan.
type Querier struct {
	inner   core.Querier
	t       *Tracer
	dialect string
	rawDB   *sql.DB
}

// NewQuerier returns a debug-recording Querier wrapper.
// rawDB is used for EXPLAIN queries: pass nil to skip EXPLAIN.
// dialect is "postgres", "mysql", or "mssql".
func NewQuerier(inner core.Querier, t *Tracer, dialect string, rawDB *sql.DB) *Querier {
	return &Querier{inner: inner, t: t, dialect: dialect, rawDB: rawDB}
}

// runExplain executes EXPLAIN for the given SELECT query and returns the plan.
// Returns empty string when rawDB is nil, dialect is unsupported, or EXPLAIN fails.
func (q *Querier) runExplain(ctx context.Context, sqlQuery string) string {
	if q.rawDB == nil {
		return ""
	}
	trimmed := strings.TrimSpace(strings.ToUpper(sqlQuery))
	if !strings.HasPrefix(trimmed, "SELECT") && !strings.HasPrefix(trimmed, "WITH") {
		return "" // EXPLAIN only useful for SELECT/CTE queries
	}

	var explainSQL string
	switch q.dialect {
	case "postgres":
		explainSQL = "EXPLAIN (FORMAT JSON) " + sqlQuery
	case "mysql":
		explainSQL = "EXPLAIN FORMAT=JSON " + sqlQuery
	case "mssql":
		explainSQL = "SET SHOWPLAN_XML ON; " + sqlQuery + "; SET SHOWPLAN_XML OFF"
	default:
		return ""
	}

	// Use a short timeout: EXPLAIN should be fast.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// For MSSQL, EXPLAIN is side-effectful (SET SHOWPLAN_XML); skip for safety.
	if q.dialect == "mssql" {
		return ""
	}

	rows, err := q.rawDB.QueryContext(ctx, explainSQL)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return ""
		}
		planLines = append(planLines, line)
	}
	if err := rows.Err(); err != nil {
		return ""
	}

	// For PG/MySQL, the result is a single JSON line. Pretty-print it.
	result := strings.Join(planLines, "\n")
	var parsed any
	if err := json.Unmarshal([]byte(result), &parsed); err == nil {
		pretty, err := json.MarshalIndent(parsed, "", "  ")
		if err == nil {
			return string(pretty)
		}
	}
	return result
}

// QueryRow wraps inner QueryRow with debug timing.
func (q *Querier) QueryRow(ctx context.Context, sql string, args ...any) (core.Row, error) {
	start := time.Now()
	row, err := q.inner.QueryRow(ctx, sql, args...)
	if err != nil {
		// core.ErrorRow, not nil: callers are allowed to scan without checking
		// the error and get it back from Scan. Returning nil would make this
		// wrapper the one Querier that panics them, and it is installed only
		// when tracing is on.
		return core.ErrorRow(err), err
	}
	dur := time.Since(start)

	detail := DBQueryDetail{
		SQL:  sql,
		Args: SanitizeArgs(args),
	}
	detail.Explain = q.runExplain(ctx, sql)
	q.t.Record(sql, KindDBQuery, dur, detail)
	return row, nil
}

// Query wraps inner Query with debug timing.
func (q *Querier) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	start := time.Now()
	rows, err := q.inner.Query(ctx, sql, args...)
	dur := time.Since(start)

	detail := DBQueryDetail{
		SQL:  sql,
		Args: SanitizeArgs(args),
	}
	if err != nil {
		q.t.RecordErr(sql, KindDBQuery, dur, detail, err)
		return rows, err
	}

	detail.Explain = q.runExplain(ctx, sql)
	q.t.Record(sql, KindDBQuery, dur, detail)
	return rows, nil
}

// Exec wraps inner Exec with debug timing.
func (q *Querier) Exec(ctx context.Context, sql string, args ...any) (core.CommandTag, error) {
	start := time.Now()
	tag, err := q.inner.Exec(ctx, sql, args...)
	dur := time.Since(start)
	ra := tag.RowsAffected
	detail := DBQueryDetail{
		SQL:          sql,
		Args:         SanitizeArgs(args),
		RowsAffected: &ra,
	}
	if err != nil {
		q.t.RecordErr(sql, KindDBQuery, dur, detail, err)
		return tag, err
	}
	q.t.Record(sql, KindDBQuery, dur, detail)
	return tag, nil
}

// Begin wraps inner Begin with debug timing.
func (q *Querier) Begin(ctx context.Context) (core.Tx, error) {
	start := time.Now()
	tx, err := q.inner.Begin(ctx)
	dur := time.Since(start)
	if err != nil {
		q.t.RecordErr("BEGIN", KindDBQuery, dur, nil, err)
		return nil, fmt.Errorf("debug querier begin: %w", err)
	}
	q.t.Record("BEGIN", KindDBQuery, dur, nil)
	return tx, nil
}

// Unwrap returns the inner Querier for type assertion chains.
func (q *Querier) Unwrap() core.Querier { return q.inner }
