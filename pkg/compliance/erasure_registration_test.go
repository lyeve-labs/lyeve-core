package compliance

import (
	"context"
	"testing"
)

type countingEraser struct {
	name  string
	calls int
}

func (e *countingEraser) EraseSubject(context.Context, string) (int64, error) {
	e.calls++
	return 1, nil
}

// A plugin may register again when reconfigured, so a second registration of
// the same eraser must not add an entry. SubjectEraserProvider gives the same
// eraser a second route in, which would otherwise double it on the first boot.
func TestRegister_SameEraserTwiceIsOneEntry(t *testing.T) {
	r := &SubjectEraserRegistry{}
	e := &countingEraser{name: "events"}

	r.Register(e)
	r.Register(e)
	r.Register(e)

	if got := r.Len(); got != 1 {
		t.Fatalf("registry holds %d erasers; want 1", got)
	}
}

// Two plugins that happen to be the same type are still two erasers.
func TestRegister_DistinctErasersAreBothKept(t *testing.T) {
	r := &SubjectEraserRegistry{}

	r.Register(&countingEraser{name: "events"})
	r.Register(&countingEraser{name: "media"})

	if got := r.Len(); got != 2 {
		t.Fatalf("registry holds %d erasers; want 2", got)
	}
}

func TestRegister_NilIsIgnored(t *testing.T) {
	r := &SubjectEraserRegistry{}
	r.Register(nil)
	if got := r.Len(); got != 0 {
		t.Fatalf("registry holds %d erasers; want 0", got)
	}
}

// An eraser whose dynamic type cannot be compared must not panic the
// registration. Every registered eraser is a pointer, so this guards a
// future one rather than a live case.
type uncomparableEraser struct{ seen map[string]bool }

func (uncomparableEraser) EraseSubject(context.Context, string) (int64, error) { return 0, nil }

func TestRegister_UncomparableEraserDoesNotPanic(t *testing.T) {
	r := &SubjectEraserRegistry{}

	r.Register(uncomparableEraser{seen: map[string]bool{}})
	r.Register(uncomparableEraser{seen: map[string]bool{}})

	if got := r.Len(); got != 2 {
		t.Fatalf("registry holds %d erasers; want both appended unchecked", got)
	}
}
