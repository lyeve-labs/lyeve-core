// WarmupConnections pre-opens connections to the database pool so the first
// wave of requests doesn't pay TCP+TLS+auth handshake cost. With MinConns=10
// and a single Ping(), only 1 connection is opened: the first 10 concurrent
// requests would still pay. Fire the configured number of concurrent pings to
// pre-fill the pool to its configured minimum.

package db

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
)

// WarmupConnections fires n concurrent pings against the pool to pre-open
// connections. Returns the number of successful pings and any aggregate errors.
// When n is 0, this is a no-op.
//
// Each ping runs in its own goroutine and must complete within the context
// deadline. Failed pings are logged but not fatal: the pool still works,
// just with fewer pre-opened connections.
func WarmupConnections(ctx context.Context, pool DB, n int) (ok, fail int) {
	if n <= 0 {
		return 0, 0
	}

	start := time.Now()

	var okCount, failCount atomic.Int64
	g, gCtx := errgroup.WithContext(ctx)
	for i := 0; i < n; i++ {
		g.Go(func() error {
			if err := pool.Ping(gCtx); err != nil {
				failCount.Add(1)
				slog.Warn("pool warmup ping failed", "err", err)
			} else {
				okCount.Add(1)
			}
			return nil // best-effort: failures counted, not fatal
		})
	}
	_ = g.Wait()

	ok = int(okCount.Load())
	fail = int(failCount.Load())

	if fail > 0 {
		slog.Warn("connection pool warmup complete - some pings failed",
			"elapsed_ms", time.Since(start).Milliseconds(),
			"ok", ok,
			"fail", fail,
			"target", n,
		)
	} else {
		slog.Info("connection pool warmup complete",
			"elapsed_ms", time.Since(start).Milliseconds(),
			"ok", ok,
			"target", n,
		)
	}

	return ok, fail
}
