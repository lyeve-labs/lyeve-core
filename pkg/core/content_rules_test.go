package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rulesEngine is a schema engine that also supplies content rules.
type rulesEngine struct {
	SchemaEngine
	calls int
}

func (r *rulesEngine) PrepareCreate(_ context.Context, _ *Schema, data map[string]any) (map[string]any, []SchemaValidationError, error) {
	r.calls++
	return data, nil, nil
}

func (r *rulesEngine) ValidatePatch(context.Context, *Schema, map[string]any, map[string]any) ([]SchemaValidationError, error) {
	r.calls++
	return nil, nil
}

// plainEngine is a schema engine that supplies no content rules.
type plainEngine struct{ SchemaEngine }

func refuses(t *testing.T, r ContentRules) {
	t.Helper()
	data, errs, err := r.PrepareCreate(context.Background(), &Schema{}, map[string]any{"a": 1})
	require.ErrorIs(t, err, ErrNoContentRules)
	assert.ErrorIs(t, err, ErrNoSchemaEngine, "an absent rule set reads as an absent engine")
	assert.Nil(t, data)
	assert.Nil(t, errs)
	errs, err = r.ValidatePatch(context.Background(), &Schema{}, map[string]any{"a": 1}, nil)
	require.ErrorIs(t, err, ErrNoContentRules)
	assert.Nil(t, errs)
}

func TestContentRulesOf_AnswersTheEnginesRulesOrRefuses(t *testing.T) {
	e := &rulesEngine{}
	assert.Same(t, e, ContentRulesOf(e))

	refuses(t, ContentRulesOf(nil))
	refuses(t, ContentRulesOf(plainEngine{}))
}

// A reader built before the engine registers follows the registration, and
// the removal after it.
func TestLazyContentRules_FollowsTheRegistration(t *testing.T) {
	var current ContentRules
	lazy := LazyContentRules(func() ContentRules { return current })
	refuses(t, lazy)

	e := &rulesEngine{}
	current = e
	_, _, err := lazy.PrepareCreate(context.Background(), &Schema{}, map[string]any{})
	require.NoError(t, err)
	_, err = lazy.ValidatePatch(context.Background(), &Schema{}, map[string]any{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, e.calls)

	current = nil
	refuses(t, lazy)
	refuses(t, LazyContentRules(nil))
}

type rulesHost struct {
	Host
	rules ContentRules
}

func (h rulesHost) ContentRules() ContentRules { return h.rules }

func TestScopedHost_ContentRulesForwardsOrRefuses(t *testing.T) {
	e := &rulesEngine{}
	assert.Same(t, e, NewScopedHost(rulesHost{rules: e}, "reader", 0).ContentRules())

	refuses(t, NewScopedHost(rulesHost{}, "reader", 0).ContentRules())
	refuses(t, NewScopedHost(struct{ Host }{}, "reader", 0).ContentRules())
}
