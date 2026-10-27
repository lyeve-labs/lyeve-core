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
