package licensing_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// verifierFunc is a licensing implementation a build could link.
type verifierFunc func(context.Context, licensing.Env) (licensing.Manager, error)

func (f verifierFunc) NewManager(ctx context.Context, env licensing.Env) (licensing.Manager, error) {
	return f(ctx, env)
}

func TestIsOpen_TellsOpenFromALinkedImplementation(t *testing.T) {
	assert.True(t, licensing.IsOpen(nil), "the engine runs a nil verifier as Open")
	assert.True(t, licensing.IsOpen(licensing.Open()))
	linked := verifierFunc(func(ctx context.Context, env licensing.Env) (licensing.Manager, error) {
		return licensing.Open().NewManager(ctx, env)
	})
	assert.False(t, licensing.IsOpen(linked), "an implementation is not Open, whatever it grants")
}
