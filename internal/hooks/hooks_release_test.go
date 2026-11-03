package hooks

import (
	"context"
	"runtime"
	"testing"
	"weak"
)

type streamBuffer struct{ records [1 << 10]byte }

// A stream registers a closure that owns its buffer, and unregisters it when
// the stream closes. The swap-remove leaves the slot past the new length in
// the backing array, so the closure has to be cleared out of it or it stays
// reachable until a later Register overwrites the slot.
func TestRegistry_UnregisterReleasesTheHandler(t *testing.T) {
	r := NewRegistry()
	r.RegisterDynamic("articles", AfterCreate, func(context.Context, Event) error { return nil })
	buf := registerStream(r)

	runtime.GC()
	if buf.Value() != nil {
		t.Fatal("an unregistered handler is still reachable from the registry")
	}
	runtime.KeepAlive(r)
}

func registerStream(r *Registry) weak.Pointer[streamBuffer] {
	buf := &streamBuffer{}
	token := r.RegisterDynamic("articles", AfterCreate, func(context.Context, Event) error {
		buf.records[0]++
		return nil
	})
	r.Unregister(token)
	return weak.Make(buf)
}

// A burst of streams on one schema grows the slice, and the array stays at
// its peak after the burst ends unless Unregister lets it go.
func TestRegistry_UnregisterShrinksAfterABurst(t *testing.T) {
	r := NewRegistry()
	var tokens []string
	for range 64 {
		tokens = append(tokens, r.RegisterDynamic("articles", AfterCreate, func(context.Context, Event) error { return nil }))
	}
	for _, tok := range tokens[2:] {
		r.Unregister(tok)
	}

	hooks := r.hooks[key("articles", AfterCreate)]
	if len(hooks) != 2 || cap(hooks) >= 8 {
		t.Fatalf("len %d cap %d after the burst, want 2 hooks in a small array", len(hooks), cap(hooks))
	}
	ran := 0
	r.Register("articles", AfterCreate, func(context.Context, Event) error { ran++; return nil })
	for _, tok := range tokens[:2] {
		r.Unregister(tok)
	}
	if err := r.Run(context.Background(), Event{Type: AfterCreate, Schema: "articles"}); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Fatalf("the remaining hook ran %d times, want 1", ran)
	}
}
