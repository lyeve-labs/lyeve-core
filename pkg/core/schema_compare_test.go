package core

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompareFieldValues_OrdersByFieldType(t *testing.T) {
	cases := []struct {
		name         string
		typeA, typeB string
		a, b         any
		want         int
	}{
		{"numbers", "number", "number", float64(2), float64(10), -1},
		{"a json number with an int", "number", "number", json.Number("10"), 2, 1},
		{"numeric strings compare as numbers", "number", "number", "9", "10", -1},
		{"an undeclared target is a number", "", "number", float64(3), float64(3), 0},
		{"dates", "date", "date", "2026-10-02", "2026-10-01", 1},
		{"dates compare as instants, not text", "datetime", "datetime", "2026-10-01T10:00:00+02:00", "2026-10-01T09:00:00Z", -1},
		{"a date with a datetime", "date", "datetime", "2026-10-01", "2026-10-01T00:00:00Z", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompareFieldValues(tc.typeA, tc.typeB, tc.a, tc.b)
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCompareFieldValues_RefusesWhatItCannotOrder(t *testing.T) {
	cases := []struct {
		name         string
		typeA, typeB string
		a, b         any
	}{
		{"a number with a date", "number", "date", float64(1), "2026-10-01"},
		{"text", "text", "text", "a", "b"},
		{"a date that does not parse", "date", "date", "yesterday", "2026-10-01"},
		{"a number that does not parse", "number", "number", "ten", float64(1)},
		{"a bool", "number", "number", true, float64(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompareFieldValues(tc.typeA, tc.typeB, tc.a, tc.b)
			assert.True(t, errors.Is(err, ErrNotComparable), "err = %v", err)
		})
	}
}

func TestCompareRuleHolds_EachRule(t *testing.T) {
	for _, tc := range []struct {
		rule       string
		lt, eq, gt bool
	}{
		{RuleLtField, true, false, false},
		{RuleLteField, true, true, false},
		{RuleGtField, false, false, true},
		{RuleGteField, false, true, true},
		{"required_with", false, false, false},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			assert.Equal(t, tc.lt, CompareRuleHolds(tc.rule, -1))
			assert.Equal(t, tc.eq, CompareRuleHolds(tc.rule, 0))
			assert.Equal(t, tc.gt, CompareRuleHolds(tc.rule, 1))
		})
	}
}
