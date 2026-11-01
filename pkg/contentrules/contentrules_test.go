package contentrules

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/i18n"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// cannedRules answers every write with the same failures, and records what it
// was asked, so a test reads what this package does with an engine's answer.
type cannedRules struct {
	fill    map[string]any
	errs    []core.SchemaValidationError
	err     error
	patched map[string]any
}

func (c *cannedRules) PrepareCreate(_ context.Context, _ *core.Schema, data map[string]any) (map[string]any, []core.SchemaValidationError, error) {
	for k, v := range c.fill {
		data[k] = v
	}
	return data, c.errs, c.err
}

func (c *cannedRules) ValidatePatch(_ context.Context, _ *core.Schema, patch, _ map[string]any) ([]core.SchemaValidationError, error) {
	c.patched = patch
	return c.errs, c.err
}

// rulesHost is a host whose registered engine supplies rules.
type rulesHost struct {
	core.Host
	rules core.ContentRules
}

func (h rulesHost) ContentRules() core.ContentRules { return h.rules }

var required = []core.SchemaValidationError{{Field: "title", Code: "VALIDATION_REQUIRED", Rule: "required"}}

func TestPrepareCreate_WritesWhatTheEngineReturns(t *testing.T) {
	host := rulesHost{rules: &cannedRules{fill: map[string]any{"status": "draft"}}}
	data, errs, err := PrepareCreate(context.Background(), host, &core.Schema{}, map[string]any{"title": "Hello"})
	require.NoError(t, err)
	assert.Empty(t, errs)
	assert.Equal(t, map[string]any{"title": "Hello", "status": "draft"}, data)
}

func TestPrepareCreate_MessageInTheRequestLocale(t *testing.T) {
	host := rulesHost{rules: &cannedRules{errs: required}}

	_, en, err := PrepareCreate(context.Background(), host, &core.Schema{}, map[string]any{})
	require.NoError(t, err)
	require.Len(t, en, 1)
	assert.Equal(t, "The field title is required.", en[0].Message)
	assert.Equal(t, "VALIDATION_REQUIRED", en[0].Code)
	assert.Equal(t, "required", en[0].Rule)

	_, fr, err := PrepareCreate(i18n.WithLocale(context.Background(), i18n.LocaleFR), host, &core.Schema{}, map[string]any{})
	require.NoError(t, err)
	require.Len(t, fr, 1)
	assert.Equal(t, "Le champ title est obligatoire.", fr[0].Message)
}

func TestValidatePatch_LocalizesTheEnginesFailures(t *testing.T) {
	rules := &cannedRules{errs: []core.SchemaValidationError{{
		Field: "rating", Code: "SCHEMA_VALIDATION_MAX", Rule: "max",
		Params: map[string]any{"max": "5", "value": "9"},
	}}}
	patch := map[string]any{"rating": float64(9)}
	errs, err := ValidatePatch(context.Background(), rulesHost{rules: rules}, &core.Schema{}, patch, nil)
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Equal(t, "The field rating must not exceed 5.", errs[0].Message)
	assert.Equal(t, patch, rules.patched)
}

// A failure the engine leaves without a code or a message still reaches the
// caller as a sentence rather than an empty message.
func TestValidatePatch_FailureWithNoCodeReadsAsAFailedField(t *testing.T) {
	host := rulesHost{rules: &cannedRules{errs: []core.SchemaValidationError{{Field: "ghost", Rule: "custom"}}}}
	errs, err := ValidatePatch(context.Background(), host, &core.Schema{}, map[string]any{"ghost": 1}, nil)
	require.NoError(t, err)
	require.Len(t, errs, 1)
	assert.Equal(t, "VALIDATION_UNKNOWN_FIELD", errs[0].Code)
	assert.Equal(t, "The field ghost failed validation.", errs[0].Message)
}

// With nothing to say whether a write is valid, the write is refused. It is
// never handed back as valid.
func TestContentRules_RefuseWithoutAnEngine(t *testing.T) {
	hosts := map[string]core.Host{
		"host without the role": rulesHost{}.Host,
		"role answers nil":      rulesHost{},
		"absent rules":          rulesHost{rules: core.AbsentContentRules{}},
	}
	for name, host := range hosts {
		t.Run(name, func(t *testing.T) {
			data, errs, err := PrepareCreate(context.Background(), host, &core.Schema{}, map[string]any{"title": "x"})
			assert.ErrorIs(t, err, core.ErrNoContentRules)
			assert.ErrorIs(t, err, core.ErrNoSchemaEngine)
			assert.Nil(t, data)
			assert.Nil(t, errs)

			errs, err = ValidatePatch(context.Background(), host, &core.Schema{}, map[string]any{"title": "x"}, nil)
			assert.ErrorIs(t, err, core.ErrNoContentRules)
			assert.Nil(t, errs)
		})
	}
}

func TestPrepareCreate_EngineErrorRefusesTheWrite(t *testing.T) {
	boom := errors.New("engine unavailable")
	data, errs, err := PrepareCreate(context.Background(), rulesHost{rules: &cannedRules{err: boom}}, &core.Schema{}, map[string]any{})
	assert.ErrorIs(t, err, boom)
	assert.Nil(t, data)
	assert.Nil(t, errs)
}
