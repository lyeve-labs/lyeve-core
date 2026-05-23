package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// The cross-field rules that order two values. Each names two targets, and
// the first is compared with the second.
const (
	RuleLtField  = "lt_field"
	RuleGtField  = "gt_field"
	RuleLteField = "lte_field"
	RuleGteField = "gte_field"
)

// IsCompareRule reports whether rule orders two field values.
func IsCompareRule(rule string) bool {
	switch rule {
	case RuleLtField, RuleGtField, RuleLteField, RuleGteField:
		return true
	}
	return false
}

// compareKind is how two values are ordered: as numbers or as instants.
type compareKind int

const (
	compareNone compareKind = iota
	compareNumber
	compareTime
)

// compareKindOf maps a field type to the way its values order. An empty type
// is a target the schema does not declare, and is compared as a number.
func compareKindOf(fieldType string) compareKind {
	switch fieldType {
	case "number", "":
		return compareNumber
	case "date", "datetime":
		return compareTime
	}
	return compareNone
}

// ErrNotComparable is returned when two values cannot be ordered: the fields
// are of types the rules cannot compare, or a value does not parse as its
// field's type.
var ErrNotComparable = errors.New("values are not comparable")

// CompareFieldValues orders a, a value of a field of typeA, against b, a value
// of a field of typeB. It answers -1, 0 or 1.
//
// A number compares with a number. A date or a datetime compares with either,
// as an instant: a date reads as midnight UTC, and a datetime without a zone
// reads as UTC. A date arrives as a string such as 2026-10-01, which no
// number parser accepts, so comparing every pair as numbers would refuse every
// write that carried two dates, whatever their order.
func CompareFieldValues(typeA, typeB string, a, b any) (int, error) {
	kind := compareKindOf(typeA)
	if kind == compareNone || kind != compareKindOf(typeB) {
		return 0, fmt.Errorf("%w: %q with %q", ErrNotComparable, typeA, typeB)
	}
	if kind == compareTime {
		ta, err := parseInstant(a)
		if err != nil {
			return 0, err
		}
		tb, err := parseInstant(b)
		if err != nil {
			return 0, err
		}
		return ta.Compare(tb), nil
	}
	fa, err := parseNumber(a)
	if err != nil {
		return 0, err
	}
	fb, err := parseNumber(b)
	if err != nil {
		return 0, err
	}
	switch {
	case fa < fb:
		return -1, nil
	case fa > fb:
		return 1, nil
	}
	return 0, nil
}

// CompareRuleHolds reports whether the ordering rule holds for cmp, the result
// of CompareFieldValues. A rule that is not an ordering rule never holds.
func CompareRuleHolds(rule string, cmp int) bool {
	switch rule {
	case RuleLtField:
		return cmp < 0
	case RuleGtField:
		return cmp > 0
	case RuleLteField:
		return cmp <= 0
	case RuleGteField:
		return cmp >= 0
	}
	return false
}

// CompareRuleRefusal returns why s may not carry rule, or the empty string
// when it may or when rule does not order two fields.
//
// A rule on two fields it cannot order is refused when the schema is saved.
// Saved, it would refuse every write that carried both fields, and the
// editor would learn that from a 422 on content rather than from the schema
// they wrote. The reason carries no sentinel, so each validator wraps it in
// its own.
func CompareRuleRefusal(s *Schema, rule CrossFieldRule) string {
	if !IsCompareRule(rule.Rule) {
		return ""
	}
	if len(rule.Targets) != 2 {
		return fmt.Sprintf("%s compares exactly two fields, got %d", rule.Rule, len(rule.Targets))
	}
	types := make([]string, 2)
	for i, name := range rule.Targets {
		f, ok := schemaFieldNamed(s, name)
		if !ok {
			return fmt.Sprintf("%s names %q, which is not a field of the schema", rule.Rule, name)
		}
		types[i] = f.FieldType
	}
	ka, kb := compareKindOf(types[0]), compareKindOf(types[1])
	if ka == compareNone || ka != kb {
		return fmt.Sprintf("%s cannot compare %q (%s) with %q (%s): both must be numbers, or both dates or datetimes",
			rule.Rule, rule.Targets[0], types[0], rule.Targets[1], types[1])
	}
	return ""
}

func schemaFieldNamed(s *Schema, name string) (SchemaField, bool) {
	if s == nil {
		return SchemaField{}, false
	}
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return SchemaField{}, false
}

// instantLayouts are the shapes a date or datetime value arrives in, the most
// specific first.
var instantLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02",
}

func parseInstant(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case *time.Time:
		if t != nil {
			return *t, nil
		}
	case string:
		for _, layout := range instantLayouts {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed, nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("%w: %v is not a date or datetime", ErrNotComparable, v)
}

func parseNumber(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, fmt.Errorf("%w: %v is not a number", ErrNotComparable, v)
		}
		return f, nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %v is not a number", ErrNotComparable, v)
		}
		return f, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	case reflect.String:
		return parseNumber(rv.String())
	}
	return 0, fmt.Errorf("%w: %v is not a number", ErrNotComparable, v)
}
