package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelEngine_FanOut(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4, Timeout: 5 * time.Second})

	items := make([]any, 100)
	for i := range items {
		items[i] = i
	}

	results, err := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		return item.(int) * 2, nil
	})

	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(results) != 100 {
		t.Errorf("expected 100 results, got %d", len(results))
	}

	// Verify all values transformed correctly.
	seen := make(map[int]bool)
	for _, r := range results {
		seen[r.Value.(int)] = true
	}
	for i := 0; i < 100; i++ {
		if !seen[i*2] {
			t.Errorf("missing result for value %d", i*2)
		}
	}
}

func TestParallelEngine_FanOutVoid(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4, Timeout: 5 * time.Second})

	items := make([]any, 50)
	for i := range items {
		items[i] = i
	}

	var counter atomic.Int32
	engine.FanOutVoid(context.Background(), items, func(ctx context.Context, item any, idx int) error {
		counter.Add(1)
		return nil
	})

	if n := counter.Load(); n != 50 {
		t.Errorf("expected 50 items processed, got %d", n)
	}
}

func TestParallelEngine_Errors(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4, Timeout: 5 * time.Second})

	items := make([]any, 10)
	for i := range items {
		items[i] = i
	}

	taskErr := errors.New("task error")
	results, firstErr := engine.FanOut(context.Background(), items, func(ctx context.Context, item any, idx int) (any, error) {
		if item.(int)%2 == 0 {
			return nil, taskErr
		}
		return item, nil
	})

	if firstErr == nil {
		t.Error("expected error, got nil")
	}
	if len(results) != 10 {
		t.Errorf("expected 10 results, got %d", len(results))
	}

	errorCount := 0
	for _, r := range results {
		if r.Err != nil {
			errorCount++
		}
	}
	if errorCount != 5 {
		t.Errorf("expected 5 errors, got %d", errorCount)
	}
}

func TestParallelEngine_SequentialFallback(t *testing.T) {
	// MaxConcurrent=1 should work (sequential).
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 1, Timeout: 5 * time.Second})

	order := make([]int, 0, 5)
	var mu sync.Mutex
	items := make([]any, 5)
	for i := range items {
		items[i] = i
	}

	engine.FanOutVoid(context.Background(), items, func(ctx context.Context, item any, idx int) error {
		time.Sleep(10 * time.Millisecond) // ensure ordering is visible
		mu.Lock()
		order = append(order, item.(int))
		mu.Unlock()
		return nil
	})

	if len(order) != 5 {
		t.Errorf("expected 5 items, got %d", len(order))
	}
}

func TestParallelEngine_EmptyInput(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4})

	results, err := engine.FanOut(context.Background(), nil, func(ctx context.Context, item any, idx int) (any, error) {
		return item, nil
	})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestParallelEngine_DefaultConfig(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{})
	if engine == nil {
		t.Fatal("expected non-nil engine")
	}
}

func TestFanOutCollect(t *testing.T) {
	engine := NewParallelEngine(ParallelConfig{MaxConcurrent: 4})

	keys := []string{"a", "b", "c"}
	result := FanOutCollect(context.Background(), engine, keys, func(ctx context.Context, key string) (string, error) {
		return key + "-result", nil
	})

	if len(result) != 3 {
		t.Errorf("expected 3 results, got %d", len(result))
	}
	if result["a"] != "a-result" {
		t.Errorf("expected a-result, got %s", result["a"])
	}
}
