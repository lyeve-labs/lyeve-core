package i18n

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

func TestLocalizeValidation_KnownCodeIsRenderedInTheLocale(t *testing.T) {
	errs := []core.SchemaValidationError{{Field: "title", Code: "VALIDATION_REQUIRED", Rule: "required", Message: "engine words"}}

	en := LocalizeValidation(LocaleEN, errs)
	require.Len(t, en, 1)
	assert.Equal(t, "The field title is required.", en[0].Message)
	assert.Equal(t, "VALIDATION_REQUIRED", en[0].Code)

	fr := LocalizeValidation(LocaleFR, errs)
	assert.Equal(t, "Le champ title est obligatoire.", fr[0].Message)
}

// A code the catalog does not hold is the engine's own, so the sentence the
// engine wrote for it is the one that reaches the caller.
func TestLocalizeValidation_UnknownCodeKeepsTheEnginesMessage(t *testing.T) {
	errs := []core.SchemaValidationError{{Field: "sku", Code: "SHOP_SKU_TAKEN", Rule: "custom", Message: "That SKU is already in use."}}
	got := LocalizeValidation(LocaleFR, errs)
	require.Len(t, got, 1)
	assert.Equal(t, "SHOP_SKU_TAKEN", got[0].Code)
	assert.Equal(t, "That SKU is already in use.", got[0].Message)
}

func TestLocalizeValidation_NoCodeAndNoMessageReadsAsAFailedField(t *testing.T) {
	got := LocalizeValidation(LocaleEN, []core.SchemaValidationError{{Field: "ghost", Rule: "custom"}})
	require.Len(t, got, 1)
	assert.Equal(t, "VALIDATION_UNKNOWN_FIELD", got[0].Code)
	assert.Equal(t, "The field ghost failed validation.", got[0].Message)

	kept := LocalizeValidation(LocaleEN, []core.SchemaValidationError{{Field: "ghost", Message: "Ghosts are not allowed."}})
	assert.Equal(t, "VALIDATION_UNKNOWN_FIELD", kept[0].Code)
	assert.Equal(t, "Ghosts are not allowed.", kept[0].Message)
}

// The engine may hand the same parameter map to several failures or keep it,
// so localizing never writes to it.
func TestLocalizeValidation_LeavesTheEnginesParametersAlone(t *testing.T) {
	params := map[string]any{"max": "5"}
	errs := []core.SchemaValidationError{{Field: "rating", Code: "SCHEMA_VALIDATION_MAX", Rule: "max", Params: params}}
	got := LocalizeValidation(LocaleEN, errs)
	require.Len(t, got, 1)
	assert.Equal(t, "The field rating must not exceed 5.", got[0].Message)
	assert.Equal(t, map[string]any{"max": "5"}, params)
	assert.Equal(t, "rating", got[0].Params["field"])
}

// The schema engine refuses a decimal with more digits than its field holds
// under the precision code, and the catalog words it from the field's
// precision and scale in the caller's locale. Both are named as values, so a
// precision of 1 or a scale of 0 leaves no plural to agree with.
func TestLocalizeValidation_PrecisionIsRenderedInTheLocale(t *testing.T) {
	tests := []struct {
		name      string
		locale    Locale
		precision int
		scale     int
		want      string
	}{
		{"en", LocaleEN, 5, 2, "The field price has a precision of 5 and a scale of 2, so it cannot hold this value."},
		{"en precision 1 scale 0", LocaleEN, 1, 0, "The field price has a precision of 1 and a scale of 0, so it cannot hold this value."},
		{"fr", LocaleFR, 5, 2, "Le champ price a une précision de 5 et une échelle de 2 : il ne peut pas contenir cette valeur."},
		{"fr precision 1 scale 0", LocaleFR, 1, 0, "Le champ price a une précision de 1 et une échelle de 0 : il ne peut pas contenir cette valeur."},
		{"fr-CA", Locale("fr-CA"), 5, 2, "Le champ price a une précision de 5 et une échelle de 2 : il ne peut pas contenir cette valeur."},
		{"ja", Locale("ja"), 5, 2, "The field price has a precision of 5 and a scale of 2, so it cannot hold this value."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := []core.SchemaValidationError{{
				Field:   "price",
				Code:    "SCHEMA_VALIDATION_PRECISION",
				Rule:    "precision",
				Value:   "123456.7",
				Message: "the schema engine's own sentence",
				Params:  map[string]any{"field": "price", "precision": tt.precision, "scale": tt.scale},
			}}
			got := LocalizeValidation(tt.locale, errs)
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].Message)
			assert.Equal(t, "SCHEMA_VALIDATION_PRECISION", got[0].Code)
			assert.Equal(t, "precision", got[0].Rule)
		})
	}
}
