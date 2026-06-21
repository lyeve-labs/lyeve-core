package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ParallelConfig configures the bounded parallel execution engine.
type ParallelConfig struct {
	// MaxConcurrent is the maximum number of goroutines to run at once.
	// Default 8.
	MaxConcurrent int
	// Timeout is the per-operation deadline. Zero means no timeout.
	Timeout time.Duration
}

// DefaultParallelConfig returns a safe default.
func DefaultParallelConfig() ParallelConfig {
	return ParallelConfig{MaxConcurrent: 8, Timeout: 30 * time.Second}
}

// MaxParallelConcurrency bounds Reconfigure. Fan-outs here are plugin
// starts, cache warmups and dashboard queries, none of which has a thousand
// items, so a ceiling above this is a mistake rather than a setting.
const MaxParallelConcurrency = 1024

// MaxParallelTimeout bounds the per-item deadline Reconfigure accepts.
const MaxParallelTimeout = time.Hour

// ErrInvalidParallelConfig is returned by Reconfigure for a config outside
// its bounds.
var ErrInvalidParallelConfig = errors.New("parallel config out of bounds: max_concurrent in [1, 1024], timeout in (0, 1h]")

// ParallelEngine runs a slice of items through a function with bounded
// parallelism. Used for parallel plugin start/stop, cache warmup, and
// dashboard query fan-out.
//
// The ceiling and the per-item deadline are set at boot and can be changed
// through Reconfigure while fan-outs run. A fan-out already in flight sees the new ceiling on
// its next acquire and the new deadline on its next item.
type ParallelEngine struct {
	sem     *semaphore
	timeout atomic.Int64 // nanoseconds. Zero means no per-item deadline
}

// MaxConcurrent returns the configured maximum concurrency.
func (e *ParallelEngine) MaxConcurrent() int { return e.sem.Limit() }

// Timeout returns the per-item deadline. Zero means none.
func (e *ParallelEngine) Timeout() time.Duration { return time.Duration(e.timeout.Load()) }

// Config returns the live configuration.
func (e *ParallelEngine) Config() ParallelConfig {
	return ParallelConfig{MaxConcurrent: e.MaxConcurrent(), Timeout: e.Timeout()}
}

// Reconfigure applies cfg to a running engine. MaxConcurrent must be in
// [1, MaxParallelConcurrency] and Timeout in (0, MaxParallelTimeout]: a
// fan-out with no deadline would let a stuck plugin start hold boot open, so
// the tunable route cannot set one, although the constructor accepts zero
// for the callers that bound their items themselves.
func (e *ParallelEngine) Reconfigure(cfg ParallelConfig) error {
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > MaxParallelConcurrency ||
		cfg.Timeout <= 0 || cfg.Timeout > MaxParallelTimeout {
		return ErrInvalidParallelConfig
	}
	e.sem.Resize(cfg.MaxConcurrent)
	e.timeout.Store(int64(cfg.Timeout))
	return nil
}

// NewParallelEngine creates a ParallelEngine. When maxConcurrent <= 0,
// defaults to 1 (sequential).
func NewParallelEngine(cfg ParallelConfig) *ParallelEngine {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	e := &ParallelEngine{sem: newSemaphore(cfg.MaxConcurrent)}
	e.timeout.Store(int64(cfg.Timeout))
	return e
}

// FanOutResult holds the result of a single item in a fan-out.
type FanOutResult[T any] struct {
	Index int
	Value T
	Err   error
}

// FanOut runs fn for each item concurrently (bounded by maxConcurrent).
// Results are returned in arbitrary order. A context cancellation aborts
// all remaining work.
func (e *ParallelEngine) FanOut(ctx context.Context, items []any, fn func(context.Context, any, int) (any, error)) ([]FanOutResult[any], error) {
	if len(items) == 0 {
		return nil, nil
	}

	var wg sync.WaitGroup
	results := make([]FanOutResult[any], 0, len(items))
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(items))

	for i, item := range items {
		idx := i
		it := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("ParallelEngine task panicked", "panic", fmt.Sprintf("%v", r))
				}
			}()

			if err := e.sem.Acquire(ctx); err != nil {
				return
			}
			defer e.sem.Release()

			taskCtx := ctx
			if timeout := e.Timeout(); timeout > 0 {
				var tCancel context.CancelFunc
				taskCtx, tCancel = context.WithTimeout(ctx, timeout)
				defer tCancel()
			}

			val, err := fn(taskCtx, it, idx)
			mu.Lock()
			results = append(results, FanOutResult[any]{Index: idx, Value: val, Err: err})
			mu.Unlock()
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	// Collect the first error, if any.
	var firstErr error
	for err := range errCh {
		if firstErr == nil {
			firstErr = err
		}
	}

	return results, firstErr
}

// FanOutVoid runs fn for each item with bounded parallelism, discarding
// results. Errors are logged but not returned. Use for fire-and-forget
// batch operations (plugin start, warmup).
func (e *ParallelEngine) FanOutVoid(ctx context.Context, items []any, fn func(context.Context, any, int) error) {
	if len(items) == 0 {
		return
	}

	var wg sync.WaitGroup
	for i, item := range items {
		idx := i
		it := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("ParallelEngine task panicked", "panic", fmt.Sprintf("%v", r))
				}
			}()

			if err := e.sem.Acquire(ctx); err != nil {
				return
			}
			defer e.sem.Release()

			taskCtx := ctx
			if timeout := e.Timeout(); timeout > 0 {
				var cancel context.CancelFunc
				taskCtx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			_ = fn(taskCtx, it, idx) // err intentionally discarded. Errors are logged by caller
		}()
	}
	wg.Wait()
}

// FanOutCollect runs fn for each string item (convenience wrapper for the
// common case of string-keyed items like plugin names). Results are a map
// of key → result. Items that errored are omitted.
func FanOutCollect[V any](ctx context.Context, engine *ParallelEngine, keys []string, fn func(context.Context, string) (V, error)) map[string]V {
	items := make([]any, len(keys))
	for i, k := range keys {
		items[i] = k
	}

	out := make(map[string]V)
	var mu sync.Mutex

	engine.FanOutVoid(ctx, items, func(taskCtx context.Context, item any, _ int) error {
		key := item.(string)
		val, err := fn(taskCtx, key)
		if err != nil {
			return err
		}
		mu.Lock()
		out[key] = val
		mu.Unlock()
		return nil
	})

	return out
}
