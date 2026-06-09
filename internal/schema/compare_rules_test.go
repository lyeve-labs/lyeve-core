package schema

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
)

// The four ordering rules compare two numbers as numbers and two dates or
// datetimes as instants, in both orders. A date arrives as a string, so
// reading every pair as numbers would refuse each write carrying two dates
// whatever their order.
func TestValidateContent_CompareRulesOrderByFieldType(t *testing.T) {
	t.Parallel()

	instant := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	pairs := []struct {
		name         string
		typeA, typeB string
		low, high    any
	}{
		{"numbers", "number", "number", float64(1), float64(5)},
		{"numeric strings", "number", "number", "1.5", "10"},
		{"dates", "date", "date", "2026-10-01", "2026-10-02"},
		{"dates across a month", "date", "date", "2026-09-30", "2026-10-01"},
		{"datetimes", "datetime", "datetime", "2026-10-01T09:00:00Z", "2026-10-01T09:00:01Z"},
		{"datetimes with offsets", "datetime", "datetime", "2026-10-01T10:00:00+02:00", "2026-10-01T09:00:00Z"},
		{"datetimes without a zone", "datetime", "datetime", "2026-10-01T09:00:00", "2026-10-01 10:00:00"},
		{"a date with a datetime", "date", "datetime", "2026-10-01", "2026-10-01T00:00:01Z"},
		{"a time value", "datetime", "datetime", instant, instant.Add(time.Second)},
	}
	rules := []struct {
		rule                string
		lowFirst, highFirst bool // whether the rule holds for (low, high) and for (high, low)
		equal               bool
	}{
		{"lt_field", true, false, false},
		{"gt_field", false, true, false},
		{"lte_field", true, false, true},
		{"gte_field", false, true, true},
	}
	for _, p := range pairs {
		for _, r := range rules {
			sc := &domain.Schema{
				Name: "events",
				Fields: []domain.SchemaField{
					{Name: "a", FieldType: p.typeA},
					{Name: "b", FieldType: p.typeB},
				},
				CrossFieldValidation: []domain.CrossFieldRule{{Rule: r.rule, Targets: []string{"a", "b"}}},
			}
			holds := func(a, b any) bool {
				return len(ValidateContent(sc, map[string]any{"a": a, "b": b})) == 0
			}
			t.Run(p.name+"/"+r.rule, func(t *testing.T) {
				assert.Equal(t, r.lowFirst, holds(p.low, p.high), "a below b")
				assert.Equal(t, r.highFirst, holds(p.high, p.low), "a above b")
				assert.Equal(t, r.equal, holds(p.low, p.low), "a equal to b")
			})
		}
	}
}

// A value that is not of its field's type, or a pair of types the rules
// cannot order, is refused rather than compared.
func TestValidateContent_CompareRulesRefuseWhatTheyCannotOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		typeA, typeB string
		a, b         any
	}{
		{"a date that does not parse", "date", "date", "first of October", "2026-10-02"},
		{"a number with a date", "number", "date", float64(1), "2026-10-02"},
		{"two text fields", "text", "text", "a", "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := &domain.Schema{
				Name: "events",
				Fields: []domain.SchemaField{
					{Name: "a", FieldType: tc.typeA},
					{Name: "b", FieldType: tc.typeB},
				},
				CrossFieldValidation: []domain.CrossFieldRule{{Rule: "lt_field", Targets: []string{"a", "b"}}},
			}
			errs := ValidateContent(sc, map[string]any{"a": tc.a, "b": tc.b})
			var rules []string
			for _, e := range errs {
				rules = append(rules, e.Rule)
			}
			assert.Contains(t, rules, "lt_field")
		})
	}
}
