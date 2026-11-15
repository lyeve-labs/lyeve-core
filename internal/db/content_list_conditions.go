package db

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/lyeve-labs/lyeve-core/internal/db/dialect"
	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/sqldialect"
)

// The list filter operators besides equality, which is a filter, and the
// bounds gt, gte, lt and lte, which are a ListRange.
const (
	OpNotEqual    = "ne"
	OpContains    = "contains"
	OpNotContains = "ncontains"
	OpIn          = "in"
	OpNotIn       = "nin"
	OpEmpty       = "empty"
)

// MaxListValues caps the values of one in or nin condition.
const MaxListValues = 50

// MaxContainsValue caps the characters of a contains or ncontains value. The
// match has no anchor, so every row is scanned against the whole value, and
// SQL Server has no statement timeout to end a long one.
const MaxContainsValue = 255

// ListCondition is one list filter with an operator. Values holds the one
// value of ne, contains, ncontains and empty, and each value of in and nin.
type ListCondition struct {
	Column string `json:"c"`
	Op     string `json:"o"`
	Values []any  `json:"v"`
}

var (
	textOperators   = []string{OpNotEqual, OpContains, OpNotContains, OpIn, OpNotIn, OpEmpty}
	numberOperators = []string{OpNotEqual, OpIn, OpNotIn, OpEmpty}
	scalarOperators = []string{OpNotEqual, OpEmpty}
	emptyOperator   = []string{OpEmpty}
)

// listOperators are the operators a filter may put on a field of each type.
// A media or json field takes empty alone: neither holds a value a person
// compares. The bounds are orderedField's question.
var listOperators = map[string][]string{
	"uid":       textOperators,
	"text":      textOperators,
	"rich_text": textOperators,
	"email":     textOperators,
	"url":       textOperators,
	"number":    numberOperators,
	"integer":   numberOperators,
	"decimal":   numberOperators,
	"relation":  numberOperators,
	"boolean":   scalarOperators,
	"date":      scalarOperators,
	"datetime":  scalarOperators,
	"media":     emptyOperator,
	"json":      emptyOperator,
}

// ListOperatorAllowed reports whether a list filter may put op on a field of
// fieldType.
func ListOperatorAllowed(fieldType, op string) bool {
	return slices.Contains(listOperators[fieldType], op)
}

// TextField reports whether a field of fieldType holds text a person wrote,
// whose empty string reads as no value.
func TextField(fieldType string) bool {
	switch fieldType {
	case "text", "rich_text", "email", "url":
		return true
	}
	return false
}

// conditionField answers the field a condition on col compares: id as a uid,
// the two system timestamps as datetimes, and the column of each field of sc
// that has one. The status and the deletion time take equality alone, and a
// to-many relation has no column here, so neither is a field for a
// condition.
func conditionField(sc *domain.Schema, col string) (domain.SchemaField, bool) {
	switch col {
	case "id":
		return domain.SchemaField{Name: col, FieldType: "uid", System: true}, true
	case "created_at", "updated_at":
		return domain.SchemaField{Name: col, FieldType: "datetime", System: true}, true
	}
	f, ok := filterField(sc, col)
	if !ok || isVirtualRelationField(f) {
		return domain.SchemaField{}, false
	}
	return f, true
}

// typedConditions answers conds with each value parsed by the type of the
// field it names, in column and operator order, so one set of conditions is
// one statement text and one cache key. An unknown column, an operator its
// field does not take, a count of values its operator does not take and a
// value its field cannot hold are domain.ErrBadRequest.
func typedConditions(sc *domain.Schema, conds []ListCondition) ([]ListCondition, error) {
	if len(conds) == 0 {
		return nil, nil
	}
	out := make([]ListCondition, 0, len(conds))
	for _, c := range conds {
		f, ok := conditionField(sc, c.Column)
		if !ok {
			return nil, fmt.Errorf("%w: unknown filter key %q", domain.ErrBadRequest, c.Column)
		}
		if !ListOperatorAllowed(f.FieldType, c.Op) {
			return nil, fmt.Errorf("%w: filter %q does not take %s", domain.ErrBadRequest, c.Column, c.Op)
		}
		n := len(c.Values)
		if c.Op == OpIn || c.Op == OpNotIn {
			if n == 0 || n > MaxListValues {
				return nil, fmt.Errorf("%w: filter %q takes 1 to %d values", domain.ErrBadRequest, c.Column, MaxListValues)
			}
		} else if n != 1 {
			return nil, fmt.Errorf("%w: filter %q takes one value", domain.ErrBadRequest, c.Column)
		}
		typed := ListCondition{Column: c.Column, Op: c.Op, Values: make([]any, n)}
		for i, v := range c.Values {
			var err error
			if typed.Values[i], err = conditionValue(f, c.Op, v); err != nil {
				return nil, err
			}
		}
		out = append(out, typed)
	}
	slices.SortFunc(out, func(a, b ListCondition) int {
		return cmp.Or(cmp.Compare(a.Column, b.Column), cmp.Compare(a.Op, b.Op))
	})
	return out, nil
}

// conditionValue parses one value of a condition. empty takes true or false.
// contains and ncontains take any text but the empty string, which every
// value contains on two dialects and none on SQL Server, up to
// MaxContainsValue characters. The others take a
// value of the field's type: text but the empty string, which empty asks
// about, a UUID for a uid or a relation, true or false for a boolean, and a
// number, a date or a datetime as a bound parses it.
func conditionValue(f domain.SchemaField, op string, v any) (any, error) {
	str, isStr := v.(string)
	bad := func(want string) error {
		return fmt.Errorf("%w: filter %q: %s takes %s", domain.ErrBadRequest, f.Name, op, want)
	}
	switch op {
	case OpEmpty:
		if str != "true" && str != "false" {
			return nil, bad("true or false")
		}
		return str == "true", nil
	case OpContains, OpNotContains:
		if !isStr || str == "" {
			return nil, bad("a value")
		}
		if utf8.RuneCountInString(str) > MaxContainsValue {
			return nil, bad(fmt.Sprintf("at most %d characters", MaxContainsValue))
		}
		return str, nil
	}
	if TextField(f.FieldType) {
		if !isStr || str == "" {
			return nil, bad("a value")
		}
		return str, nil
	}
	switch f.FieldType {
	case "uid", "relation":
		id, err := uuid.Parse(str)
		if !isStr || err != nil {
			return nil, bad("a UUID")
		}
		return id.String(), nil
	case "boolean":
		if str != "true" && str != "false" {
			return nil, bad("true or false")
		}
		return str == "true", nil
	case "date", "datetime":
		return rangeValue(f, v)
	}
	return filterValue(f, v)
}

// conditionClause answers the predicate for c over the table aliased r, with
// its first placeholder $n, and the values it binds. ne, ncontains and nin
// also keep an entry whose value is empty, because "is not X" reads as
// including the blank ones. An operator it does not know is an error, never
// a predicate it guessed.
func conditionClause(engine string, sc *domain.Schema, c ListCondition, n int) (string, []any, error) {
	d := dialect.Must(engine)
	f, _ := conditionField(sc, c.Column)
	col := "r." + d.QuoteIdentifier(c.Column)
	blank := blankExpr(engine, f, col)
	switch c.Op {
	case OpEmpty:
		if c.Values[0] == true {
			return blank, nil, nil
		}
		return "NOT " + blank, nil, nil
	case OpContains, OpNotContains:
		match := sqldialect.ILike(d.Name(), textExpr(engine, f, col), fmt.Sprintf("$%d", n)) + sqldialect.ILikeEscapeClause(d.Name())
		args := []any{sqldialect.ILikeValue(d.Name(), c.Values[0].(string))}
		if c.Op == OpContains {
			return match, args, nil
		}
		return "(" + blank + " OR NOT (" + match + "))", args, nil
	case OpIn, OpNotIn:
		params := make([]string, len(c.Values))
		for k := range c.Values {
			params[k] = filterParam(engine, sc, c.Column, n+k)
		}
		list := strings.Join(params, ", ")
		if c.Op == OpIn {
			return col + " IN (" + list + ")", c.Values, nil
		}
		return "(" + blank + " OR " + col + " NOT IN (" + list + "))", c.Values, nil
	case OpNotEqual:
		return "(" + blank + " OR " + col + " <> " + filterParam(engine, sc, c.Column, n) + ")", c.Values, nil
	}
	return "", nil, fmt.Errorf("content list: no predicate for operator %q", c.Op)
}

// blankExpr answers the parenthesized predicate that holds when col has no
// value: NULL, for a text column the empty string too, for a media column an
// empty list as the store writes it, and for a json column a JSON null, an
// empty string, an empty list or an empty object. The length test stands in
// for an equality with the empty string on MySQL and SQL Server, whose
// collations can pad a comparison with spaces, so a value of spaces would
// read as empty there and nowhere else.
func blankExpr(engine string, f domain.SchemaField, col string) string {
	switch f.FieldType {
	case "json":
		return "(" + col + " IS NULL OR " + emptyJSON(engine, col) + ")"
	case "text", "rich_text", "email", "url", "media":
	default:
		return "(" + col + " IS NULL)"
	}
	empty := col + " = ''"
	switch engine {
	case "mysql":
		empty = "CHAR_LENGTH(" + col + ") = 0"
	case "mssql":
		empty = "DATALENGTH(" + col + ") = 0"
	}
	if f.FieldType == "media" {
		empty += " OR " + col + " = '[]'"
	}
	return "(" + col + " IS NULL OR " + empty + ")"
}

// emptyJSON answers the predicate that holds when the json column col holds
// a JSON null, an empty string, an empty list or an empty object. PostgreSQL
// compares jsonb values, which ignore spacing. MySQL reads the value's type
// and length. SQL Server keeps JSON as the text the store wrote, which
// json.Marshal writes without spaces.
func emptyJSON(engine, col string) string {
	switch engine {
	case "mysql":
		return "JSON_TYPE(" + col + ") = 'NULL' OR (JSON_TYPE(" + col + ") IN ('ARRAY', 'OBJECT') AND JSON_LENGTH(" + col + ") = 0) OR " + col + " = CAST('\"\"' AS JSON)"
	case "mssql":
		return col + " IN (N'null', N'\"\"', N'[]', N'{}')"
	}
	return col + " IN ('null'::jsonb, '\"\"'::jsonb, '[]'::jsonb, '{}'::jsonb)"
}

// textExpr answers col as text for a match. A uid is a UUID column on
// PostgreSQL, which has no ILIKE, and on SQL Server, where CHARINDEX would
// convert the value it looks for into a UUID. MySQL keeps it as text.
func textExpr(engine string, f domain.SchemaField, col string) string {
	if f.FieldType != "uid" {
		return col
	}
	switch engine {
	case "postgres":
		return "CAST(" + col + " AS TEXT)"
	case "mssql":
		return "CAST(" + col + " AS NVARCHAR(36))"
	}
	return col
}
