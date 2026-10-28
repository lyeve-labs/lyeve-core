package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A name no plugin registered is not found, so the schema engine decides what
// it means rather than the registry inventing a validator for it.
func TestCustomValidator_UnclaimedNameIsNotFound(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)

	fn, ok := LookupCustomValidator("nobody-claims-this")
	assert.False(t, ok)
	assert.Nil(t, fn)
}

func TestCustomValidator_APluginNameResolvesToItsFunction(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)

	RegisterCustomValidator("postcode", func(value any, body string, params map[string]any) CustomValidatorResult {
		if value == "SW1A 1AA" {
			return CustomValidatorResult{OK: true}
		}
		return CustomValidatorResult{Error: "not a postcode we deliver to"}
	})

	fn, ok := LookupCustomValidator("postcode")
	require.True(t, ok)

	res := fn("SW1A 1AA", `\d+`, nil)
	assert.True(t, res.OK)
	assert.Equal(t, "not a postcode we deliver to", fn("nope", `\d+`, nil).Error)
	assert.Equal(t, []string{"postcode"}, RegisteredCustomValidators())
}

func TestCustomValidator_RegistryRefusesAnEntryThatCannotRun(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)

	RegisterCustomValidator("", func(any, string, map[string]any) CustomValidatorResult { return CustomValidatorResult{OK: true} })
	RegisterCustomValidator("nil-fn", nil)

	assert.Empty(t, RegisteredCustomValidators(),
		"a stored entry that cannot run is worse than none: the schema would name a validator that does nothing")
}
