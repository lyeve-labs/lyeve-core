package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/core/enginehost"
	"github.com/lyeve-labs/lyeve-core/pkg/engine"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// leaderPlugin takes a cluster lock in Start the way the singleton plugins
// do (a scheduler, a relay, a sweeper) and keeps it until Stop. With keep
// false it releases the lock before Start returns, the way a one-off
// migration guarded by a lock does.
type leaderPlugin struct {
	name string
	keep bool

	mu   sync.Mutex
	lock *engine.DistLock
	held bool
}

func (p *leaderPlugin) Name() string { return p.name }

func (p *leaderPlugin) Start(ctx context.Context, host core.Host) error {
	lock := host.DistLock(p.name)
	held, err := lock.TryAcquire(ctx, host.RawDB(), host.Dialect())
	if err != nil {
		return fmt.Errorf("%s: acquire lock: %w", p.name, err)
	}
	if !held {
		return fmt.Errorf("%s: lock held elsewhere", p.name)
	}
	var one int
	if err := host.RawDB().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		_ = lock.Release()
		return fmt.Errorf("%s: query while holding the lock: %w", p.name, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = true
	if !p.keep {
		return lock.Release()
	}
	p.lock = lock
	return nil
}

func (p *leaderPlugin) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lock == nil {
		return nil
	}
	return p.lock.Release()
}

// Every singleton plugin keeps its cluster lock while it leads. If those
// sessions came from the two-connection pool RawDB serves, the first two
// leaders would take both connections and the next plugin to ask for a lock
// would block in Start for good, so the engine would never start listening.
// Boot has to finish with far more leaders than that pool has connections,
// and RawDB has to keep answering once it has.
func TestBoot_LeaderLocksDoNotStarveRawDB_Postgres(t *testing.T) {
	if !testdb.ShouldTest("postgres") {
		t.Skip("CI_DIALECT excludes postgres")
	}
	pool, dsn := testdb.PostgresWithDSN(t)
	testBootWithLeaderLocks(t, pool, dsn)
}

func TestBoot_LeaderLocksDoNotStarveRawDB_MySQL(t *testing.T) {
	if !testdb.ShouldTest("mysql") {
		t.Skip("CI_DIALECT excludes mysql")
	}
	pool, dsn := testdb.MySQLWithDSN(t)
	testBootWithLeaderLocks(t, pool, dsn)
}

func TestBoot_LeaderLocksDoNotStarveRawDB_MSSQL(t *testing.T) {
	if !testdb.ShouldTest("mssql") {
		t.Skip("CI_DIALECT excludes mssql")
	}
	pool, dsn := testdb.MSSQLWithDSN(t)
	testBootWithLeaderLocks(t, pool, dsn)
}

func testBootWithLeaderLocks(t *testing.T, pool db.DB, dsn string) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	const leaders = 8
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	host := enginehost.NewHost(pool, sqlDBOf(t, pool), &config.Config{}, nil, "test")
	attachSessionPools(context.Background(), host, pool.Engine(), dsn, logger)
	require.NotSame(t, sqlDBOf(t, pool), host.RawDB(), "RawDB must serve the guarded pool, as the engine does")

	suffix := time.Now().UnixNano()
	var plugins []*leaderPlugin
	var features []string
	for i := range leaders + 1 {
		p := &leaderPlugin{name: fmt.Sprintf("bootlock-%d-%d", suffix, i), keep: i < leaders}
		plugins = append(plugins, p)
		features = append(features, p.name)
		plugin.RegisterPluginWithCaps(p.name, func() core.Plugin { return p }, core.CapRawDB)
	}
	t.Cleanup(func() {
		for _, p := range plugins {
			plugin.UnregisterPlugin(p.name)
		}
	})

	a := plugin.NewActivator(host, logger)
	a.Resolve(granting(features), "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Start(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatalf("boot did not finish within 60s with %d plugins holding a lock", leaders)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	for _, p := range plugins {
		p.mu.Lock()
		held := p.held
		p.mu.Unlock()
		require.True(t, held, "plugin %s never took its lock", p.name)
	}

	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer qcancel()
	var one int
	require.NoError(t, host.RawDB().QueryRowContext(qctx, "SELECT 1").Scan(&one),
		"RawDB must still serve a query while every leader holds its lock")
}
