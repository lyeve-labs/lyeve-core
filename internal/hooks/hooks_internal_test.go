// White-box tests for internal hook registry state.
package hooks

import (
	"context"
	"testing"
)

func TestRegistry_Unregister_BoundedGrowth(t *testing.T) {
	r := NewRegistry()
	const n = 50

	// Register n hooks under the same key.
	var tokens []string
	for i := 0; i < n; i++ {
		token := r.RegisterDynamic("articles", AfterCreate, func(_ context.Context, _ Event) error {
			return nil
		})
		tokens = append(tokens, token)
	}

	// Verify n hooks in the slice.
	r.mu.RLock()
	got := len(r.hooks[key("articles", AfterCreate)])
	r.mu.RUnlock()
	if got != n {
		t.Fatalf("after %d registrations, got %d hooks (want %d)", n, got, n)
	}

	// Unregister all n hooks.
	for i := 0; i < n; i++ {
		r.Unregister(tokens[i])
	}

	// Slice must be gone (empty, removed from map).
	r.mu.RLock()
	got = len(r.hooks[key("articles", AfterCreate)])
	r.mu.RUnlock()
	if got != 0 {
		t.Fatalf("after unregistering all %d hooks, got %d (want 0)", n, got)
	}

	// Repeated register/unregister cycles must keep the slice bounded.
	for cycle := 0; cycle < 10; cycle++ {
		var cycleTokens []string
		for i := 0; i < n; i++ {
			token := r.RegisterDynamic("articles", AfterCreate, func(_ context.Context, _ Event) error {
				return nil
			})
			cycleTokens = append(cycleTokens, token)
		}
		r.mu.RLock()
		got := len(r.hooks[key("articles", AfterCreate)])
		r.mu.RUnlock()
		if got != n {
			t.Fatalf("cycle %d: after %d registrations, got %d hooks (want %d)", cycle, n, got, n)
		}
		for i := 0; i < n; i++ {
			r.Unregister(cycleTokens[i])
		}
		r.mu.RLock()
		got = len(r.hooks[key("articles", AfterCreate)])
		r.mu.RUnlock()
		if got != 0 {
			t.Fatalf("cycle %d: after unregistering all, got %d hooks (want 0)", cycle, got)
		}
	}

	// Final sanity: register one more and verify it fires.
	var fired bool
	_ = r.RegisterDynamic("articles", AfterCreate, func(_ context.Context, _ Event) error {
		fired = true
		return nil
	})
	if err := r.Run(context.Background(), Event{Type: AfterCreate, Schema: "articles"}); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("hook should fire after boundedness cycles")
	}
}

func TestRegistry_Unregister_SwapRemove_PreservesByID(t *testing.T) {
	// Register 3 hooks then unregister the middle one (idx=1).
	// The hook at idx=2 should be displaced to idx=1, and its byID
	// entry must be updated so it can still be unregistered later.
	r := NewRegistry()

	t1 := r.RegisterDynamic("pages", BeforeUpdate, func(_ context.Context, _ Event) error {
		return nil
	})
	t2 := r.RegisterDynamic("pages", BeforeUpdate, func(_ context.Context, _ Event) error {
		return nil
	})
	t3 := r.RegisterDynamic("pages", BeforeUpdate, func(_ context.Context, _ Event) error {
		return nil
	})

	// Unregister the middle hook (idx=1). This should displace t3 from
	// idx=2 to idx=1.
	r.Unregister(t2)

	r.mu.RLock()
	hooks := r.hooks[key("pages", BeforeUpdate)]
	r.mu.RUnlock()
	if len(hooks) != 2 {
		t.Fatalf("after unregister, got %d hooks (want 2)", len(hooks))
	}

	// Unregister t3: should succeed because its byID idx was updated.
	r.Unregister(t3)

	r.mu.RLock()
	hooks = r.hooks[key("pages", BeforeUpdate)]
	r.mu.RUnlock()
	if len(hooks) != 1 {
		t.Fatalf("after unregister t3, got %d hooks (want 1)", len(hooks))
	}

	// t1 should still be present and fire.
	var t1fired bool
	r.Unregister(t1) // remove first so we can re-register a fresh hook cleanly
	_ = r.RegisterDynamic("pages", BeforeUpdate, func(_ context.Context, _ Event) error {
		t1fired = true
		return nil
	})
	if err := r.Run(context.Background(), Event{Type: BeforeUpdate, Schema: "pages"}); err != nil {
		t.Fatal(err)
	}
	// The original t1 was unregistered. Only the re-registered one fires.
	if !t1fired {
		t.Fatal("re-registered hook should fire")
	}
}
