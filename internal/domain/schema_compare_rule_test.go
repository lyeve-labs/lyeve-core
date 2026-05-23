package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// An ordering rule on two fields it cannot compare would refuse every write
// that carried both, so the schema that declares it is refused when saved.
func TestValidateSchema_CompareRuleNeedsComparableFields(t *testing.T) {
	cases := []struct {
		name         string
		typeA, typeB string
		targets      []string
		ok           bool
	}{
		{"two numbers", "number", "number", []string{"a", "b"}, true},
		{"two dates", "date", "date", []string{"a", "b"}, true},
		{"two datetimes", "datetime", "datetime", []string{"a", "b"}, true},
		{"a date with a datetime", "date", "datetime", []string{"a", "b"}, true},
		{"a number with a date", "number", "date", []string{"a", "b"}, false},
		{"two text fields", "text", "text", []string{"a", "b"}, false},
		{"a field the schema does not have", "number", "number", []string{"a", "missing"}, false},
		{"one target", "number", "number", []string{"a"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Schema{
				Name: "events",
				Fields: []SchemaField{
					{Name: "a", FieldType: tc.typeA},
					{Name: "b", FieldType: tc.typeB},
				},
				CrossFieldValidation: []CrossFieldRule{{Rule: "gt_field", Targets: tc.targets}},
			}
			err := ValidateSchema(s)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.True(t, errors.Is(err, ErrValidation), "err = %v", err)
		})
	}
}

// required_with names fields of any type, so the ordering check leaves it
// alone.
func TestValidateSchema_RequiredWithIsNotAnOrderingRule(t *testing.T) {
	s := &Schema{
		Name:                 "events",
		Fields:               []SchemaField{{Name: "a", FieldType: "text"}, {Name: "b", FieldType: "json"}},
		CrossFieldValidation: []CrossFieldRule{{Rule: "required_with", Targets: []string{"a", "b"}}},
	}
	assert.NoError(t, ValidateSchema(s))
}
