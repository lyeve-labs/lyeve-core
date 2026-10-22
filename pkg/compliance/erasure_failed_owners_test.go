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

// signingOutAccount is the account eraser, recording its sign-out and its
// erasure in order.
type signingOutAccount struct{ order *[]string }

func (a *signingOutAccount) ErasesAccount() {}

func (a *signingOutAccount) EraserName() string { return "account" }

func (a *signingOutAccount) EndSessions(context.Context, string) error {
	*a.order = append(*a.order, "sign-out")
	return nil
}

func (a *signingOutAccount) EraseSubject(context.Context, string) (int64, error) {
	*a.order = append(*a.order, "account")
	return 1, nil
}

// orderedEraser records its run in order, and fails with err when it is set.
type orderedEraser struct {
	name  string
	err   error
	order *[]string
}

func (o *orderedEraser) EraserName() string { return o.name }

func (o *orderedEraser) EraseSubject(context.Context, string) (int64, error) {
	*o.order = append(*o.order, o.name)
	return 1, o.err
}

// The account eraser rewrites the address the other owners resolve the
// subject by. Run while one of them failed, it left the next run nothing to
// find. The subject is signed out first, and the account is erased only once
// every other eraser has succeeded.
func TestRunSubjectErasure_SignsOutFirstAndErasesTheAccountLast(t *testing.T) {
	t.Run("every eraser succeeds", func(t *testing.T) {
		var order []string
		reg := &SubjectEraserRegistry{}
		reg.Register(&signingOutAccount{order: &order})
		reg.Register(&orderedEraser{name: "email", order: &order})

		res, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
		require.NoError(t, err)
		assert.Equal(t, []string{"sign-out", "email", "account"}, order)
		assert.Empty(t, res.Failed)
	})

	t.Run("another eraser fails", func(t *testing.T) {
		var order []string
		reg := &SubjectEraserRegistry{}
		reg.Register(&signingOutAccount{order: &order})
		reg.Register(&orderedEraser{name: "email", err: errors.New("down"), order: &order})
		reg.Register(&orderedEraser{name: "media", order: &order})

		res, err := runSubjectErasureOn(context.Background(), reg, "someone@example.test")
		require.EqualError(t, err, "down")
		assert.Equal(t, []string{"sign-out", "email", "media"}, order,
			"the subject is signed out and the others still run, and the account is not erased")
		assert.Equal(t, []string{"email", "account"}, res.Failed, "the account is named as not run")
	})
}
