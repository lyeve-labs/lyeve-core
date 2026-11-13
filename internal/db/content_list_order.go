package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/internal/tenant"
)

// ListSort names the column a list is ordered by and its direction. The zero
// value keeps the list's default order.
type ListSort struct {
	Column string
	Desc   bool
}

// String answers the sort as a query writes it: the column, with a leading
// minus when descending.
func (o ListSort) String() string {
	if o.Desc {
		return "-" + o.Column
	}
	return o.Column
}

// ListRange bounds a column inclusively. A nil bound leaves that side open.
type ListRange struct {
	GTE any `json:"gte,omitempty"`
	LTE any `json:"lte,omitempty"`
}

// ListQuery is one page of an offset list: exact-match filters and ranges,
// both keyed by column, and the order.
type ListQuery struct {
	Limit   int
	Offset  int
	Filters map[string]any
	Ranges  map[string]ListRange
	Sort    ListSort
}

// orderBy answers the ORDER BY terms for o over the table aliased r. A row
// with no value comes last in both directions on every dialect: PostgreSQL
// puts NULL first in a descending order and MySQL and SQL Server in an
// ascending one, so a nullable column leads with a term of its own. The id
// breaks a tie in the same direction, so the order is total and a page
// boundary inside a run of equal values neither repeats nor skips a row.
func (o ListSort) orderBy(d dialect.Dialect) string {
	dir := " ASC"
	if o.Desc {
		dir = " DESC"
	}
	col := "r." + d.QuoteIdentifier(o.Column)
	terms := col + dir + ", r.id" + dir
	if !systemTimestamp(o.Column) {
		terms = "CASE WHEN " + col + " IS NULL THEN 1 ELSE 0 END, " + terms
	}
	return terms
}

// systemTimestamp reports whether col is one of the two timestamps every
// entry carries. Both are NOT NULL, and the tenant index on (tenant_id,
// created_at) serves an order on created_at.
func systemTimestamp(col string) bool {
	return col == "created_at" || col == "updated_at"
}

// orderedField answers the field a list may sort or bound by col: the two
// system timestamps, and a number, integer, decimal, date or datetime field
// of sc. A text field is not one. Its order follows each dialect's collation,
// so one request would answer three orders, and an unindexed text column
// cannot be a key column on MySQL or SQL Server. An empty col answers no
// field and no error.
func orderedField(sc *domain.Schema, col string) (domain.SchemaField, error) {
	if col == "" {
		return domain.SchemaField{}, nil
	}
	if systemTimestamp(col) {
		return domain.SchemaField{Name: col, FieldType: "datetime", System: true}, nil
	}
	if f, ok := filterField(sc, col); ok {
		switch f.FieldType {
		case "number", "integer", "decimal", "date", "datetime":
			return f, nil
		}
	}
	return domain.SchemaField{}, fmt.Errorf("%w: cannot sort or bound by %q", domain.ErrBadRequest, col)
}

// typedRanges answers ranges with each bound parsed by the type of the field
// it names, in a new map. A column orderedField refuses, and a bound its field
// cannot hold, is domain.ErrBadRequest.
func typedRanges(sc *domain.Schema, ranges map[string]ListRange) (map[string]ListRange, error) {
	if len(ranges) == 0 {
		return nil, nil
	}
	out := make(map[string]ListRange, len(ranges))
	for col, r := range ranges {
		f, err := orderedField(sc, col)
		if err != nil {
			return nil, err
		}
		var typed ListRange
		if typed.GTE, err = rangeValue(f, r.GTE); err != nil {
			return nil, err
		}
		if typed.LTE, err = rangeValue(f, r.LTE); err != nil {
			return nil, err
		}
		out[col] = typed
	}
	return out, nil
}

// rangeValue parses one bound by its field's type: a number, an integer and
// a decimal as an equality filter parses them, a date written 2006-01-02, and
// a datetime in any form a write accepts, read as UTC.
func rangeValue(f domain.SchemaField, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.FieldType {
	case "date":
		str, ok := v.(string)
		if _, err := time.Parse(time.DateOnly, str); !ok || err != nil {
			return nil, fmt.Errorf("%w: filter %q: not a date", domain.ErrBadRequest, f.Name)
		}
		return str, nil
	case "datetime":
		str, _ := v.(string)
		at, ok := ParseContentTime(str)
		if !ok {
			return nil, fmt.Errorf("%w: filter %q: not a datetime", domain.ErrBadRequest, f.Name)
		}
		return at, nil
	}
	return filterValue(f, v)
}

// listCursorSorted is ListCursorSorted for a sort that names a column.
func (s *ContentStore) listCursorSorted(ctx context.Context, sc *domain.Schema, table, cursor string, limit int, drafts bool, sort ListSort) ([]*domain.Content, error) {
	if _, err := orderedField(sc, sort.Column); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	engine := s.pool.Engine()
	d := dialect.Must(engine)
	limitClause := "LIMIT $1"
	if engine == "mssql" {
		limitClause = "OFFSET 0 ROWS FETCH NEXT $1 ROWS ONLY"
	}
	args := []any{limit, tenant.ID(ctx)}
	where := "r.tenant_id = $2" + softDeleteWhere(sc) + draftWhere(sc, "r", drafts)
	if cursor != "" {
		cursorID, err := uuid.Parse(cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid cursor", domain.ErrBadRequest)
		}
		args = append(args, cursorID)
		after, err := s.afterCursor(ctx, d, table, sort, cursorID)
		if err != nil {
			return nil, err
		}
		where += " AND " + after
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, created_at, updated_at,
		       %s
		FROM %s r
		WHERE %s
		ORDER BY %s
		%s`, dataColumnExpr(engine, sc, "r"), table, where, sort.orderBy(d), limitClause), args...)
	if err != nil {
		return nil, fmt.Errorf("list cursor %s: %w", table, err)
	}
	defer rows.Close()

	items := make([]*domain.Content, 0)
	for rows.Next() {
		c, scanErr := scanFullRow(engine, rows, sc.Name, sc)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// afterCursor answers the predicate that keeps the rows after the cursor row
// in sort's order, with $2 the tenant and $3 the cursor's id. The cursor
// row's value is read by a subquery, so it is compared as stored: a number
// read back through JSON is a float64 and would lose digits. Whether that
// value is NULL picks the predicate, because the rows after a NULL are the
// NULLs with a later id, and the rows after a value include every NULL.
func (s *ContentStore) afterCursor(ctx context.Context, d dialect.Dialect, table string, sort ListSort, cursorID uuid.UUID) (string, error) {
	qcol := d.QuoteIdentifier(sort.Column)
	row, err := s.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT CASE WHEN c.%s IS NULL THEN 1 ELSE 0 END FROM %s c WHERE c.id = $1 AND c.tenant_id = $2`, qcol, table),
		cursorID, tenant.ID(ctx))
	if err != nil {
		return "", fmt.Errorf("list cursor %s: %w", table, err)
	}
	var isNull int
	if err := row.Scan(&isNull); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: the cursor names no entry", domain.ErrBadRequest)
		}
		return "", fmt.Errorf("list cursor %s: %w", table, err)
	}
	cmp := ">"
	if sort.Desc {
		cmp = "<"
	}
	col := "r." + qcol
	if isNull == 1 {
		return fmt.Sprintf("%s IS NULL AND r.id %s $3", col, cmp), nil
	}
	value := fmt.Sprintf("(SELECT c.%s FROM %s c WHERE c.id = $3 AND c.tenant_id = $2)", qcol, table)
	after := fmt.Sprintf("%s %s %s OR (%s = %s AND r.id %s $3)", col, cmp, value, col, value, cmp)
	if !systemTimestamp(sort.Column) {
		after = col + " IS NULL OR " + after
	}
	return "(" + after + ")", nil
}
