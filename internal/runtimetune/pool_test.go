package runtimetune_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// startConns is the pool size the tests' engine started with, the cap
// plugintest gives every test pool.
const startConns = 4

// poolTuner builds a tuner over host's pool, with a resolver that reads the
// test's environment.
func poolTuner(t *testing.T, host core.Host) (*runtimetune.Tuner, *sql.DB) {
	t.Helper()
	sqlDB := host.RawDB()
	require.NotNil(t, sqlDB)
	sqlDB.SetMaxOpenConns(startConns)
	sqlDB.SetMaxIdleConns(startConns)
	t.Cleanup(func() {
		sqlDB.SetMaxOpenConns(startConns)
		sqlDB.SetMaxIdleConns(startConns)
	})
	return runtimetune.New(runtimetune.Options{
		Root:     machine("max 100000", "max"),
		DB:       sqlDB,
		Dialect:  host.Dialect(),
		Config:   &config.Config{DatabaseMaxConns: startConns},
		Resolver: config.NewResolver(nil),
	}), sqlDB
}

func TestSetDatabaseMaxConns_ResizesTheRunningPool(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		clean(t)
		tuner, sqlDB := poolTuner(t, host)
		assert.Equal(t, core.SourceDefault, tuner.RuntimeSettings().DatabaseMaxConns.Source)

		view, err := tuner.SetDatabaseMaxConns(7)
		require.NoError(t, err)
		assert.Equal(t, 7, sqlDB.Stats().MaxOpenConnections)
		assert.Equal(t, 7, view.DatabaseMaxConns.Value)
		assert.Equal(t, core.SourceSet, view.DatabaseMaxConns.Source)

		view, err = tuner.SetDatabaseMaxConns(0)
		require.NoError(t, err)
		assert.Equal(t, startConns, sqlDB.Stats().MaxOpenConnections, "0 returns the pool to its start size")
		assert.Equal(t, core.SourceDefault, view.DatabaseMaxConns.Source)

		for _, n := range []int{-1, core.MaxDatabaseConns + 1} {
			_, err = tuner.SetDatabaseMaxConns(n)
			assert.Error(t, err, "n = %d", n)
		}
		assert.Equal(t, startConns, sqlDB.Stats().MaxOpenConnections)
	})
}

// Backpressure reads the pool's cap from its live stats on each poll, so its
// pressure follows a resize without a restart.
func TestSetDatabaseMaxConns_BackpressureUsesTheNewCap(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		clean(t)
		tuner, sqlDB := poolTuner(t, host)
		bp, err := apimw.Backpressure(apimw.BackpressureConfig{
			MaxInflight:      10,
			PoolPollInterval: 5 * time.Millisecond,
			PoolStats: func() apimw.PoolStats {
				s := sqlDB.Stats()
				return apimw.PoolStats{MaxOpenConnections: s.MaxOpenConnections, InUse: s.InUse}
			},
		})
		require.NoError(t, err)
		t.Cleanup(bp.Stop)

		ctx := context.Background()
		for range 2 {
			conn, err := sqlDB.Conn(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
		}
		require.Eventually(t, func() bool { return bp.Stats().PoolPressure == 0.5 }, 5*time.Second, 5*time.Millisecond,
			"two of four connections in use")

		_, err = tuner.SetDatabaseMaxConns(8)
		require.NoError(t, err)
		require.Eventually(t, func() bool { return bp.Stats().PoolPressure == 0.25 }, 5*time.Second, 5*time.Millisecond,
			"two of eight connections in use")
	})
}

func TestSetDatabaseMaxConns_PinnedUnlessOpened(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		t.Run("pinned", func(t *testing.T) {
			clean(t)
			t.Setenv("DATABASE_MAX_CONNECTIONS", "4")
			tuner, sqlDB := poolTuner(t, host)

			_, err := tuner.SetDatabaseMaxConns(7)
			requirePinned(t, err, "DATABASE_MAX_CONNECTIONS")
			assert.Equal(t, startConns, sqlDB.Stats().MaxOpenConnections)
			s := tuner.RuntimeSettings().DatabaseMaxConns
			assert.True(t, s.Pinned)
			assert.Equal(t, core.SourceEnvironment, s.Source)
		})
		t.Run("opened by LYEVE_OVERRIDABLE", func(t *testing.T) {
			clean(t)
			t.Setenv("DATABASE_MAX_CONNECTIONS", "4")
			t.Setenv(config.OverridableEnvKey, "DATABASE_MAX_CONNECTIONS")
			tuner, sqlDB := poolTuner(t, host)

			view, err := tuner.SetDatabaseMaxConns(7)
			require.NoError(t, err)
			assert.Equal(t, 7, sqlDB.Stats().MaxOpenConnections)
			assert.Equal(t, core.Setting{Value: 7, Source: core.SourceSet}, view.DatabaseMaxConns,
				"a written value no longer names the variable it replaced")
		})
	})
}

// A pool that claims more than a third of what the database accepts gets
// the warning the boot logs, in the view the resize answers.
func TestSetDatabaseMaxConns_WarnsWhenThePoolCrowdsTheServer(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		clean(t)
		tuner, _ := poolTuner(t, host)

		view, err := tuner.SetDatabaseMaxConns(5)
		require.NoError(t, err)
		assert.Empty(t, view.Warning, "five connections crowd no server")

		view, err = tuner.SetDatabaseMaxConns(core.MaxDatabaseConns)
		require.NoError(t, err)
		if host.Dialect() == "mssql" {
			// SQL Server reports 32767 for its dynamic limit, a third of which
			// is above the bound.
			assert.Empty(t, view.Warning)
		} else {
			assert.Contains(t, view.Warning, "more than a third")
		}
	})
}

// Each dialect reads its own connection limit, sessions and cores, and SQL
// Server's default of no fixed limit reads as that rather than as a failure.
func TestRuntimeFacts_Database(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		clean(t)
		tuner, _ := poolTuner(t, host)
		f := tuner.RuntimeFacts(context.Background())

		assert.Equal(t, host.Dialect(), f.Dialect)
		require.True(t, f.DatabaseConnsOpen.Known, "connections open: %s", f.DatabaseConnsOpen.Reason)
		assert.GreaterOrEqual(t, f.DatabaseConnsOpen.Value, 1)
		switch host.Dialect() {
		case "mssql":
			assert.False(t, f.DatabaseConnLimit.Known)
			assert.Equal(t, core.FactReasonNoFixedLimit, f.DatabaseConnLimit.Reason)
			require.True(t, f.DatabaseCores.Known, "cores: %s", f.DatabaseCores.Reason)
			assert.GreaterOrEqual(t, f.DatabaseCores.Value, 1)
		default:
			require.True(t, f.DatabaseConnLimit.Known, "connection limit: %s", f.DatabaseConnLimit.Reason)
			assert.Greater(t, f.DatabaseConnLimit.Value, 1)
			assert.False(t, f.DatabaseCores.Known)
			assert.NotEmpty(t, f.DatabaseCores.Reason)
		}
	})
}
