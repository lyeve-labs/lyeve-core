package engine

import (
	"context"
	"testing"
	"time"
)

// The executor promises that a hook is not aborted when the caller goes away.
// The pool decides whether to start a task by reading the context it was
// handed, so the context has to be detached before submit, or a hook queued
// when its caller disconnected would be dropped. after_create hooks are on
// that path.
func TestAsyncHookExecutor_RunsAHookWhoseCallerAlreadyWentAway(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{Size: 1})
	t.Cleanup(pool.Shutdown)

	e := NewAsyncHookExecutor(pool, AsyncHookConfig{Timeout: 5 * time.Second})
	if !e.Enabled() {
		t.Fatal("the executor must dispatch through the pool for this to mean anything")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := make(chan struct{})
	e.Run(ctx, "after_create", HookContext{TenantID: "acme"}, func(hookCtx context.Context) error {
		if hookCtx.Err() != nil {
			t.Error("the hook must not inherit the caller's cancellation")
		}
		close(ran)
		return nil
	})

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook never ran: the pool refused it because the caller's context was already done")
	}

	if d := pool.Dropped(); d != 0 {
		t.Errorf("the hook must not be counted as work the pool declined, got dropped=%d", d)
	}
}
