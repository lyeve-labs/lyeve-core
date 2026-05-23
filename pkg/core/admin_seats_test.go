package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// checkOnlyGuard answers CheckAdminSeat and holds nothing still.
type checkOnlyGuard struct{ err error }

func (g checkOnlyGuard) CheckAdminSeat(context.Context, uuid.UUID, []string) error { return g.err }

// writerGuard is a guard that holds the count still: it answers err before
// the write when refuse is set, and otherwise runs the write and then answers
// commitErr, as a commit that fails after the write succeeded would.
type writerGuard struct {
	checkOnlyGuard
	refuse    bool
	commitErr error
}

func (g writerGuard) WithAdminSeat(ctx context.Context, _ uuid.UUID, _ []string, write func(context.Context) error) error {
	if g.refuse {
		return g.err
	}
	if err := write(ctx); err != nil {
		return err
	}
	return g.commitErr
}

func TestWriteWithAdminSeat_TellsRefusalFromWriteFailure(t *testing.T) {
	full := &core.AdminSeatCapError{Limit: 2, Current: 2}
	writeFailed := errors.New("insert failed")
	commitFailed := errors.New("commit failed")

	cases := []struct {
		name      string
		guard     core.AdminSeatGuard
		writeErr  error
		wantRan   bool
		wantSeat  error
		wantWrite error
	}{
		{name: "no guard runs the write", guard: nil, wantRan: true},
		{name: "check-only guard refuses", guard: checkOnlyGuard{err: full}, wantSeat: full},
		{name: "check-only guard admits", guard: checkOnlyGuard{}, wantRan: true},
		{name: "check-only guard admits a failing write", guard: checkOnlyGuard{}, writeErr: writeFailed, wantRan: true, wantWrite: writeFailed},
		{name: "writer refuses before the write", guard: writerGuard{checkOnlyGuard: checkOnlyGuard{err: full}, refuse: true}, wantSeat: full},
		{name: "writer admits", guard: writerGuard{}, wantRan: true},
		{name: "writer admits a failing write", guard: writerGuard{}, writeErr: writeFailed, wantRan: true, wantWrite: writeFailed},
		{name: "writer fails to commit", guard: writerGuard{commitErr: commitFailed}, wantRan: true, wantWrite: commitFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			seatErr, writeErr := core.WriteWithAdminSeat(context.Background(), tc.guard, uuid.Nil, []string{"admin"}, func(context.Context) error {
				ran = true
				return tc.writeErr
			})
			assert.Equal(t, tc.wantRan, ran)
			assert.Equal(t, tc.wantSeat, seatErr)
			assert.Equal(t, tc.wantWrite, writeErr)
		})
	}
}

// serializingHost stands in for a host that can take the lock.
type serializingHost struct{ keys []string }

func (h *serializingHost) SerializeWrite(ctx context.Context, key string, fn func(context.Context) error) error {
	h.keys = append(h.keys, key)
	return fn(ctx)
}

func TestSerializeWrite_UsesTheHostLockWhenThereIsOne(t *testing.T) {
	h := &serializingHost{}
	ran := 0
	fn := func(context.Context) error { ran++; return nil }

	assert.NoError(t, core.SerializeWrite(context.Background(), h, "flow.flows:acme", fn))
	assert.Equal(t, []string{"flow.flows:acme"}, h.keys)

	assert.NoError(t, core.SerializeWrite(context.Background(), struct{}{}, "flow.flows:acme", fn))
	assert.Equal(t, 2, ran, "a host with no lock still runs the write")
}
