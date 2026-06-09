package plugintest

import (
	"database/sql"
	"sync"
	"sync/atomic"

	"github.com/lyeve-labs/lyeve-core/internal/metrics"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The engine's metrics registry is one per process, and the pool collector
// on it registers once. The runtime does both at boot. The test host does
// the same on first use, reading pool stats from whichever host's database
// was opened last, so a plugin test gathers the same series a running
// engine exports, lyeve_db_pool_* included, without a stand-in registry.
var (
	metricsOnce sync.Once
	metricsDB   atomic.Pointer[sql.DB]
)

// MetricsGatherer implements core.MetricsGathererProvider over the engine's
// own registry.
func (h *testHost) MetricsGatherer() core.MetricsGatherer {
	if h.rawDB != nil {
		metricsDB.Store(h.rawDB)
	}
	metricsOnce.Do(func() {
		reg := metrics.Init()
		reg.MustRegister(metrics.NewDBCollector(func() metrics.DBStats {
			db := metricsDB.Load()
			if db == nil {
				return metrics.DBStats{}
			}
			stat := db.Stats()
			return metrics.DBStats{
				MaxOpenConnections: stat.MaxOpenConnections,
				OpenConnections:    stat.OpenConnections,
				InUse:              stat.InUse,
				Idle:               stat.Idle,
				WaitCount:          stat.WaitCount,
				WaitDuration:       stat.WaitDuration.Seconds(),
				MaxIdleClosed:      stat.MaxIdleClosed,
				MaxIdleTimeClosed:  stat.MaxIdleTimeClosed,
				MaxLifetimeClosed:  stat.MaxLifetimeClosed,
			}
		}))
	})
	return metrics.Registry()
}

var _ core.MetricsGathererProvider = (*testHost)(nil)
