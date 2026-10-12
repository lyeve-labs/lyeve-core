package contentrules

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// articleSchema has a required field with a default, a required field
// without one, an enum, a bounded number and a cross-field rule.
func articleSchema() *core.Schema {
	return &core.Schema{
		Name: "articles",
		Fields: []core.SchemaField{
			{Name: "id", FieldType: "uid", System: true},
			{Name: "title", FieldType: "text", Required: true},
			{Name: "status", FieldType: "text", Required: true, Default: "draft",
				Validation: []core.ValidationRule{{Rule: "enum", Params: map[string]any{"values": []any{"draft", "live"}}}}},
			{Name: "rating", FieldType: "number", Validation: []core.ValidationRule{{Rule: "max", Params: map[string]any{"max": 5}}}},
			{Name: "starts", FieldType: "number"},
			{Name: "ends", FieldType: "number"},
		},
		CrossFieldValidation: []core.CrossFieldRule{{Rule: "lt_field", Targets: []string{"starts", "ends"}}},
	}
}

// failures reduces each failure to field and rule, which is what a test can
// hold steady across message wording.
func failures(errs []core.SchemaValidationError) [][2]string {
	out := make([][2]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, [2]string{e.Field, e.Rule})
	}
	return out
}

func TestPrepareCreate_FillsDefaultsBeforeValidating(t *testing.T) {
	data, errs := PrepareCreate(context.Background(), articleSchema(), map[string]any{"title": "Hello"})
	assert.Empty(t, errs, "a required field with a default is satisfied by leaving it out")
	assert.Equal(t, "draft", data["status"])
}

func TestPrepareCreate_RefusesWhatTheContentAPIRefuses(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want [][2]string
	}{
		{"required field missing", map[string]any{}, [][2]string{{"title", "required"}}},
		{"explicit null is kept, not defaulted", map[string]any{"title": "x", "status": nil}, [][2]string{{"status", "required"}}},
		{"enum", map[string]any{"title": "x", "status": "gone"}, [][2]string{{"status", "enum"}}},
		{"max", map[string]any{"title": "x", "rating": float64(9)}, [][2]string{{"rating", "max"}}},
		{"field type", map[string]any{"title": true}, [][2]string{{"title", "field_type"}}},
		{"cross-field", map[string]any{"title": "x", "starts": float64(5), "ends": float64(1)}, [][2]string{{"starts,ends", "less_than_field"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := PrepareCreate(context.Background(), articleSchema(), tc.data)
			assert.Equal(t, tc.want, failures(errs))
			for _, e := range errs {
				assert.NotEmpty(t, e.Message)
				assert.NotEmpty(t, e.Code)
			}
		})
	}
}

func TestValidatePatch_ChecksOnlyWhatThePatchCarries(t *testing.T) {
	// The stored row breaks two rules already. A patch that touches neither
	// field is not refused for them.
	current := map[string]any{"title": "", "rating": float64(9), "starts": float64(1), "ends": float64(3)}
	assert.Empty(t, ValidatePatch(context.Background(), articleSchema(), map[string]any{"status": "live"}, current))

	cases := []struct {
		name  string
		patch map[string]any
		want  [][2]string
	}{
		{"null on a required field", map[string]any{"title": nil}, [][2]string{{"title", "required"}}},
		{"null on an optional field", map[string]any{"rating": nil}, [][2]string{}},
		{"a value the rule refuses", map[string]any{"status": "gone"}, [][2]string{{"status", "enum"}}},
		{"one side of a cross-field rule against the stored other", map[string]any{"starts": float64(7)}, [][2]string{{"starts,ends", "less_than_field"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, failures(ValidatePatch(context.Background(), articleSchema(), tc.patch, current)))
		})
	}
}

func TestPrepareCreate_MessageInTheRequestLocale(t *testing.T) {
	_, en := PrepareCreate(context.Background(), articleSchema(), map[string]any{})
	require.Len(t, en, 1)
	assert.Equal(t, "The field title is required.", en[0].Message)
	assert.Equal(t, "VALIDATION_REQUIRED", en[0].Code)

	_, fr := PrepareCreate(i18n.WithLocale(context.Background(), i18n.LocaleFR), articleSchema(), map[string]any{})
	require.Len(t, fr, 1)
	assert.Equal(t, "Le champ title est obligatoire.", fr[0].Message)
}
