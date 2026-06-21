package engine

import (
	"context"
	"testing"
)

func TestAsyncHookExecutor_Disabled(t *testing.T) {
	exec := MakeAsyncHookExecutor(nil)
	if exec.Enabled() {
		t.Error("expected disabled executor when pool is nil")
	}

	// Should not panic, runs synchronously.
	called := false
	exec.Run(context.Background(), "test", HookContext{}, func(ctx context.Context) error {
		called = true
		return nil
	})
	if !called {
		t.Error("expected hook to be called")
	}
}

func TestAsyncHookExecutor_DefaultConfig(t *testing.T) {
	cfg := DefaultAsyncHookConfig()
	if cfg.Timeout == 0 {
		t.Error("expected non-zero timeout")
	}
}
