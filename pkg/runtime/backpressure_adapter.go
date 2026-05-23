package runtime

import (
	"database/sql"

	mw "github.com/lyeve-labs/lyeve-core/internal/middleware"
)

// poolStatsAdapter wraps a func() sql.DBStats into the middleware.PoolStatsFunc
// expected by middleware.Backpressure.
func poolStatsAdapter(statsFn func() sql.DBStats) mw.PoolStatsFunc {
	return func() mw.PoolStats {
		s := statsFn()
		return mw.PoolStats{
			MaxOpenConnections: s.MaxOpenConnections,
			OpenConnections:    s.OpenConnections,
			InUse:              s.InUse,
			WaitCount:          s.WaitCount,
			WaitDuration:       s.WaitDuration,
		}
	}
}
