package compliance

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// namedEraser fails with err when it is set, and cancels the run after it
// erases when cancel is set.
type namedEraser struct {
	name   string
	err    error
	cancel context.CancelFunc
}

func (n *namedEraser) EraserName() string { return n.name }

func (n *namedEraser) EraseSubject(context.Context, string) (int64, error) {
	if n.cancel != nil {
		n.cancel()
	}
	if n.err != nil {
		return 0, n.err
	}
	return 1, nil
}

// The run returned only the first error, so a caller recording an incomplete
// erasure could name one owner when several were left. The result now names
// every eraser that failed and every one that never ran.
func TestRunSubjectErasure_NamesEveryOwnerThatFailedOrNeverRan(t *testing.T) {
	t.Run("every failure, in fan-out order", func(t *testing.T) {
		reg := &SubjectEraserRegistry{}
		reg.Register(&namedEraser{name: "email", err: errors.New("smtp store down")})
		reg.Register(&namedEraser{name: "content"})
		reg.Register(&namedEraser{name: "media", err: errors.New("bucket refused")})

		res, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
		require.EqualError(t, err, "smtp store down", "the error stays the first one")
		assert.Equal(t, []string{"email", "media"}, res.Failed)
		assert.EqualValues(t, 1, res.Rows)
	})

	t.Run("the erasers a canceled run never reached", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reg := &SubjectEraserRegistry{}
		reg.Register(&namedEraser{name: "content", cancel: cancel})
		reg.Register(&namedEraser{name: "media"})
		reg.Register(&namedEraser{name: "email"})

		res, err := runSubjectErasureOn(ctx, reg, "someone@example.test")
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, []string{"media", "email"}, res.Failed)
		assert.EqualValues(t, 1, res.Rows)
	})

	t.Run("a run canceled before it starts names them all", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reg := &SubjectEraserRegistry{}
		reg.Register(&namedEraser{name: "content"})
		reg.Register(&namedEraser{name: "media"})

		res, err := runSubjectErasureOn(ctx, reg, "someone@example.test")
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, []string{"content", "media"}, res.Failed)
	})

	t.Run("a complete run names none", func(t *testing.T) {
		reg := &SubjectEraserRegistry{}
		reg.Register(&namedEraser{name: "content"})

		res, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
		require.NoError(t, err)
		assert.Empty(t, res.Failed)
	})

	t.Run("through the exported call", func(t *testing.T) {
		ResetSubjectErasers()
		t.Cleanup(ResetSubjectErasers)
		ResetHoldChecker()
		t.Cleanup(ResetHoldChecker)
		RegisterSubjectEraser(&namedEraser{name: "forms", err: errors.New("down")})
		RegisterSubjectEraser(&namedEraser{name: "content"})

		res, err := RunSubjectErasureWithHolds(context.Background(), "someone@example.test")
		require.Error(t, err)
		assert.Equal(t, []string{"forms"}, res.Failed)
	})
}
