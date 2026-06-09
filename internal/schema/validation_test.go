package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/domain"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestValidateContent_RefusesValuesTheColumnCannotHold(t *testing.T) {
	t.Parallel()

	s := &domain.Schema{
		Name: "probe",
		Fields: []domain.SchemaField{
			{Name: "label", FieldType: "text"},
			{Name: "flag", FieldType: "boolean"},
			{Name: "count", FieldType: "number"},
			{Name: "blob", FieldType: "json"},
			{Name: "untyped"},
		},
	}

	cases := []struct {
		name     string
		data     map[string]any
		field    string
		expected string
	}{
		{
			name:     "a boolean for a text column",
			data:     map[string]any{"label": false},
			field:    "label",
			expected: "text",
		},
		{
			name:     "a number for a text column",
			data:     map[string]any{"label": float64(42)},
			field:    "label",
			expected: "text",
		},
		{
			name:     "a string for a boolean column",
			data:     map[string]any{"flag": "yes"},
			field:    "flag",
			expected: "boolean",
		},
		{
			name:     "a number for a boolean column",
			data:     map[string]any{"flag": float64(1)},
			field:    "flag",
			expected: "boolean",
		},
		{
			name:     "a boolean for a number column",
			data:     map[string]any{"count": true},
			field:    "count",
			expected: "number",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := ValidateContent(s, tc.data)
			require.Len(t, errs, 1)
			assert.Equal(t, tc.field, errs[0].Field)
			assert.Equal(t, "field_type", errs[0].Rule)
			assert.Equal(t, tc.expected, errs[0].Params["expected"])
		})
	}
}

func TestValidateContent_AcceptsWhatTheColumnDoesHold(t *testing.T) {
	t.Parallel()

	s := &domain.Schema{
		Name: "probe",
		Fields: []domain.SchemaField{
			{Name: "label", FieldType: "text"},
			{Name: "flag", FieldType: "boolean"},
			{Name: "count", FieldType: "number"},
			{Name: "blob", FieldType: "json"},
			{Name: "untyped"},
		},
	}

	cases := []struct {
		name string
		data map[string]any
	}{
		{"a string for text", map[string]any{"label": "x"}},
		{"a boolean for boolean", map[string]any{"flag": true}},
		{"a number for number", map[string]any{"count": float64(3)}},
		{"anything for json", map[string]any{"blob": map[string]any{"a": 1}}},
		{"a boolean for json", map[string]any{"blob": true}},
		// An object for a text column is stored as its JSON text, and callers
		// depend on it, so it stays accepted.
		{"an object for text", map[string]any{"label": map[string]any{"a": 1}}},
		// A field that declares no type describes nothing to check against, and
		// the admin write path stores the body as JSON rather than binding it
		// to a column, so nothing is refused on its behalf.
		{"a string for an untyped field", map[string]any{"untyped": "x"}},
		{"a number for an untyped field", map[string]any{"untyped": float64(1)}},
		{"a boolean for an untyped field", map[string]any{"untyped": true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Empty(t, ValidateContent(s, tc.data))
		})
	}
}

// A custom validator's body is read as a pattern when no plugin claims the
// name, so a definition like this validates on an install with no plugin at
// all rather than refusing every value.
func TestValidateContent_CustomValidatorRunsFromItsBody(t *testing.T) {
	s := &domain.Schema{
		Name: "probe",
		Fields: []domain.SchemaField{{
			Name:       "sku",
			FieldType:  "text",
			Validation: []domain.ValidationRule{{Rule: "custom:sku_shape"}},
		}},
		CustomValidators: []domain.CustomValidator{
			{Name: "sku_shape", Body: `[A-Z]{2}\d{4}`},
		},
	}

	require.Empty(t, ValidateContent(s, map[string]any{"sku": "AB1234"}))

	errs := ValidateContent(s, map[string]any{"sku": "not-a-sku"})
	require.Len(t, errs, 1)
	assert.Equal(t, "sku", errs[0].Field)
	assert.Contains(t, errs[0].Message, "does not match")
	assert.NotContains(t, errs[0].Message, "is not registered",
		"a validator with a body runs from it rather than being refused as unregistered")
}

// A plugin that claims the name decides what it means, and its answer replaces
// the pattern rather than running beside it.
func TestValidateContent_APluginValidatorReplacesTheBody(t *testing.T) {
	core.ResetCustomValidators()
	t.Cleanup(core.ResetCustomValidators)
	core.RegisterCustomValidator("sku_shape",
		func(value any, body string, params map[string]any) core.CustomValidatorResult {
			return core.CustomValidatorResult{Error: "the plugin says no"}
		})

	s := &domain.Schema{
		Name: "probe",
		Fields: []domain.SchemaField{{
			Name:       "sku",
			FieldType:  "text",
			Validation: []domain.ValidationRule{{Rule: "custom:sku_shape"}},
		}},
		CustomValidators: []domain.CustomValidator{
			{Name: "sku_shape", Body: `[A-Z]{2}\d{4}`},
		},
	}

	errs := ValidateContent(s, map[string]any{"sku": "AB1234"})
	require.Len(t, errs, 1, "the body would have accepted this, so the plugin's answer is what ran")
	assert.Equal(t, "the plugin says no", errs[0].Message)
}

// A rule naming a validator the schema never declared is still an error, and
// still names the validator rather than the value.
func TestValidateContent_UndeclaredCustomValidatorIsStillRefused(t *testing.T) {
	s := &domain.Schema{
		Name: "probe",
		Fields: []domain.SchemaField{{
			Name:       "sku",
			FieldType:  "text",
			Validation: []domain.ValidationRule{{Rule: "custom:never_declared"}},
		}},
	}

	errs := ValidateContent(s, map[string]any{"sku": "anything"})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Message, "unknown custom validator")
}

func TestValidatePatch_ChecksOnlyThePresentFields(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "title", FieldType: "text", Required: true, Validation: []domain.ValidationRule{
				{Rule: "max", Params: map[string]any{"max": 5}},
			}},
			{Name: "status", FieldType: "text", Required: true, Validation: []domain.ValidationRule{
				{Rule: "enum", Params: map[string]any{"values": []any{"draft", "live"}}},
			}},
			{Name: "starts", FieldType: "number"},
			{Name: "ends", FieldType: "number"},
		},
		CrossFieldValidation: []domain.CrossFieldRule{{Rule: "lt_field", Targets: []string{"starts", "ends"}}},
	}
	current := map[string]any{"title": "Hi", "status": "draft", "starts": float64(1), "ends": float64(5)}

	cases := []struct {
		name  string
		patch map[string]any
		rules []string
	}{
		{"an omitted required field is not checked", map[string]any{"status": "live"}, nil},
		{"an empty patch passes", map[string]any{}, nil},
		{"an explicit null for a required field is refused", map[string]any{"title": nil}, []string{"required"}},
		{"an empty string for a required field is refused", map[string]any{"status": ""}, []string{"required"}},
		{"a present field outside its enum is refused", map[string]any{"status": "gone"}, []string{"enum"}},
		{"a present field over its length is refused", map[string]any{"title": "too long"}, []string{"max"}},
		{"a present field of the wrong type is refused", map[string]any{"title": true}, []string{"field_type"}},
		{"a cross-field rule reads the stored partner", map[string]any{"starts": float64(9)}, []string{"less_than_field"}},
		{"a cross-field rule passes against the stored partner", map[string]any{"ends": float64(9)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, e := range ValidatePatch(sc, tc.patch, current) {
				got = append(got, e.Rule)
			}
			assert.Equal(t, tc.rules, got)
		})
	}
}

func TestValidatePatch_StoredViolationIsNotChargedToAnUnrelatedPatch(t *testing.T) {
	sc := &domain.Schema{
		Fields: []domain.SchemaField{
			{Name: "starts", FieldType: "number"},
			{Name: "ends", FieldType: "number"},
			{Name: "note", FieldType: "text"},
		},
		CrossFieldValidation: []domain.CrossFieldRule{{Rule: "lt_field", Targets: []string{"starts", "ends"}}},
	}
	current := map[string]any{"starts": float64(9), "ends": float64(1)}
	assert.Empty(t, ValidatePatch(sc, map[string]any{"note": "x"}, current))
}

// A field typed email must hold an address whether or not the schema also
// names the email rule. Without the check MySQL and SQL Server would store any
// string, and only PostgreSQL would refuse it at the column.
func TestValidateContent_EmailFieldTypeIsAnAddress(t *testing.T) {
	t.Parallel()

	typed := &domain.Schema{Name: "probe", Fields: []domain.SchemaField{{Name: "contact", FieldType: "email"}}}
	both := &domain.Schema{Name: "probe", Fields: []domain.SchemaField{{
		Name: "contact", FieldType: "email", Validation: []domain.ValidationRule{{Rule: "email"}},
	}}}

	for _, tc := range []struct {
		name   string
		schema *domain.Schema
		value  any
		errs   int
	}{
		{"a malformed address on a typed field", typed, "not-an-address", 1},
		{"a well-formed address on a typed field", typed, "ada@example.com", 0},
		{"a malformed address with the rule named too is reported once", both, "not-an-address", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateContent(tc.schema, map[string]any{"contact": tc.value})
			require.Len(t, errs, tc.errs, "%+v", errs)
			for _, e := range errs {
				assert.Equal(t, "email", e.Rule)
				assert.Equal(t, "contact", e.Field)
			}
		})
	}

	patchErrs := ValidatePatch(typed, map[string]any{"contact": "nope"}, map[string]any{"contact": "ada@example.com"})
	require.Len(t, patchErrs, 1, "a partial update that sets a malformed address is refused")
}
