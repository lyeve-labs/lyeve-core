package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomValidator_PatternIsTheFallback(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)

	fn, ok := LookupCustomValidator("nobody-claims-this")
	require.True(t, ok, "an unclaimed name must still resolve, or the schema refuses every value")

	assert.True(t, fn("AB1234", `[A-Z]{2}\d{4}`, nil).OK)
	assert.False(t, fn("ab1234", `[A-Z]{2}\d{4}`, nil).OK)
}

// A body is anchored, so a validator means the whole value. Without this a
// pattern accepts anything that merely contains a match, which reads as
// working until the first value with a prefix.
func TestCustomValidator_PatternMatchesTheWholeValue(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)
	fn, _ := LookupCustomValidator("x")

	assert.False(t, fn("xxAB1234yy", `[A-Z]{2}\d{4}`, nil).OK)
	assert.True(t, fn("AB1234", `^[A-Z]{2}\d{4}$`, nil).OK, "an already anchored body is not double anchored")
}

func TestCustomValidator_PatternRefusesAnUnusableBody(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)
	fn, _ := LookupCustomValidator("x")

	res := fn("anything", "[unterminated", nil)
	assert.False(t, res.OK)
	assert.Contains(t, res.Error, "not usable")

	empty := fn("anything", "", nil)
	assert.False(t, empty.OK)
	assert.Contains(t, empty.Error, "nothing to check")
}

// A non-string is compared by its printed form, and a nil value is left to the
// required rule rather than failed here.
func TestCustomValidator_PatternOnNonStrings(t *testing.T) {
	ResetCustomValidators()
	t.Cleanup(ResetCustomValidators)
	fn, _ := LookupCustomValidator("x")

	assert.True(t, fn(42, `\d+`, nil).OK)
	assert.False(t, fn(42, `[A-Z]+`, nil).OK)
	assert.True(t, fn(nil, `[A-Z]+`, nil).OK, "an absent value is the required rule's business")
}

func TestCustomValidator_APluginNameWinsOverThePattern(t *testing.T) {
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

	// The body would refuse this under the pattern interpreter, so a pass
	// proves the registered function ran instead.
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
