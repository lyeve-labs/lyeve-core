package runtimetune_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	apimw "github.com/lyeve-labs/lyeve-core/internal/middleware"
	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
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
			// SQL Server's default sets no fixed limit to crowd.
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
		requireReasonCodes(t, f)

		assert.Equal(t, host.Dialect(), f.Dialect)
		require.True(t, f.DatabaseConnsOpen.Known, "connections open: %s", f.DatabaseConnsOpen.Reason)
		assert.GreaterOrEqual(t, f.DatabaseConnsOpen.Value, 1)
		switch host.Dialect() {
		case "mssql":
			assert.False(t, f.DatabaseConnLimit.Known)
			assert.Equal(t, core.FactReasonNoFixedLimit, f.DatabaseConnLimit.ReasonCode)
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

// asAccount opens a pool on the database at dsn as a new account that holds
// no server-wide grant, and drops the account when the test ends. admin runs
// the statements that create and drop it.
func asAccount(t *testing.T, admin *sql.DB, dsn, driver, account string, create, drop []string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range create {
		_, err := admin.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(account, factsPassword)
	sqlDB, err := sql.Open(driver, u.String())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = sqlDB.Close()
		for _, stmt := range drop {
			_, _ = admin.ExecContext(ctx, stmt) // best effort: the test database is dropped after
		}
	})
	return sqlDB
}

// factsPassword is the password of the account asAccount creates.
const factsPassword = "Facts-Reader-9x"

// factsAccount names an account no other test on the shared server uses,
// since accounts belong to the server rather than to one database.
func factsAccount() string {
	return fmt.Sprintf("runtime_facts_%d", time.Now().UnixNano())
}

// An account that cannot see other accounts' sessions would count only its
// own, so the count reads as unknown with the grant it needs, never as a
// small number that looks known.
func TestRuntimeFacts_SessionsUnknownWithoutTheGrant(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		pool, dsn := testdb.PostgresWithDSN(t)
		account := factsAccount()
		sqlDB := asAccount(t, pool.SQLDB(), dsn, "pgx", account,
			[]string{"CREATE ROLE " + account + " LOGIN PASSWORD '" + factsPassword + "'"},
			[]string{"DROP ROLE IF EXISTS " + account})
		tuner := runtimetune.New(runtimetune.Options{Root: machine("max 100000", "max"), DB: sqlDB, Dialect: "postgres", Resolver: config.NewResolver(nil)})

		f := tuner.RuntimeFacts(context.Background())
		requireReasonCodes(t, f)
		assert.False(t, f.DatabaseConnsOpen.Known, "connections open read as %v", f.DatabaseConnsOpen.Value)
		assert.Equal(t, core.FactReasonMissingGrant, f.DatabaseConnsOpen.ReasonCode)
		assert.Equal(t, "pg_read_all_stats", f.DatabaseConnsOpen.Grant)
		assert.Contains(t, f.DatabaseConnsOpen.Reason, "pg_read_all_stats")
		assert.True(t, f.DatabaseConnLimit.Known, "connection limit: %s", f.DatabaseConnLimit.Reason)
	})
	t.Run("mssql", func(t *testing.T) {
		pool, dsn := testdb.MSSQLWithDSN(t)
		account := factsAccount()
		sqlDB := asAccount(t, pool.SQLDB(), dsn, "sqlserver", account,
			[]string{
				"CREATE LOGIN " + account + " WITH PASSWORD = '" + factsPassword + "', CHECK_POLICY = OFF",
				"CREATE USER " + account + " FOR LOGIN " + account,
			},
			[]string{"DROP USER IF EXISTS " + account, "DROP LOGIN " + account})
		tuner := runtimetune.New(runtimetune.Options{Root: machine("max 100000", "max"), DB: sqlDB, Dialect: "mssql", Resolver: config.NewResolver(nil)})

		f := tuner.RuntimeFacts(context.Background())
		requireReasonCodes(t, f)
		for name, fact := range map[string]core.Fact{"connections open": f.DatabaseConnsOpen, "cores": f.DatabaseCores} {
			assert.False(t, fact.Known, "%s read as %v", name, fact.Value)
			assert.Equal(t, "VIEW SERVER STATE", fact.Grant, name)
			assert.Contains(t, fact.Reason, "VIEW SERVER STATE", name)
		}
	})
}

// reasonUnreadable is the reason a fact gives when its read failed.
const reasonUnreadable = "the database did not answer the read"

// A read that fails, here on a canceled context, reads as a failed read,
// never as a missing grant that would send the operator after a permission.
func TestRuntimeFacts_FailedReadIsNotAMissingGrant(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		clean(t)
		tuner, _ := poolTuner(t, host)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		f := tuner.RuntimeFacts(ctx)
		requireReasonCodes(t, f)
		facts := map[string]core.Fact{"connection limit": f.DatabaseConnLimit, "connections open": f.DatabaseConnsOpen}
		if host.Dialect() == "mssql" {
			facts["cores"] = f.DatabaseCores
		}
		for name, fact := range facts {
			assert.False(t, fact.Known, name)
			assert.Equal(t, core.FactReasonUnreadable, fact.ReasonCode, name)
			assert.Equal(t, reasonUnreadable, fact.Reason, name)
		}
	})
}

// runtimeFacts reads the facts and fails the test when the read is not back
// within limit.
func runtimeFacts(t *testing.T, tuner *runtimetune.Tuner, limit time.Duration) core.RuntimeFacts {
	t.Helper()
	done := make(chan core.RuntimeFacts, 1)
	go func() { done <- tuner.RuntimeFacts(context.Background()) }()
	select {
	case f := <-done:
		return f
	case <-time.After(limit):
		t.Fatalf("the facts were not back after %s", limit)
		return core.RuntimeFacts{}
	}
}

// A pool with no connection free answers the facts after a bound of their
// own rather than the caller's whole deadline.
func TestRuntimeFacts_BoundedWhenThePoolIsFull(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		clean(t)
		tuner, sqlDB := poolTuner(t, host)
		sqlDB.SetMaxOpenConns(1)
		conn, err := sqlDB.Conn(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		f := runtimeFacts(t, tuner, 10*time.Second)
		requireReasonCodes(t, f)
		assert.Equal(t, reasonUnreadable, f.DatabaseConnLimit.Reason)
		assert.Equal(t, reasonUnreadable, f.DatabaseConnsOpen.Reason)
	})
}

// hangingReplicas is a roster that answers only when its context ends.
type hangingReplicas struct{}

func (hangingReplicas) Replicas(ctx context.Context) (int, bool) { <-ctx.Done(); return 0, false }
func (hangingReplicas) Attached() bool                           { return true }

func TestRuntimeFacts_BoundedWhenTheRosterHangs(t *testing.T) {
	clean(t)
	tuner := runtimetune.New(runtimetune.Options{Root: machine("max 100000", "max"), Replicas: hangingReplicas{}, Resolver: config.NewResolver(nil)})

	f := runtimeFacts(t, tuner, 10*time.Second)
	requireReasonCodes(t, f)
	assert.Equal(t, core.FactReasonNoRoster, f.Replicas.ReasonCode)
}

// The crowding check reads the limit the facts report, which on PostgreSQL
// leaves out the reserved slots. A pool between a third of that limit and a
// third of max_connections crowds the server.
func TestSetDatabaseMaxConns_CrowdingLeavesOutReservedSlots(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		if host.Dialect() != "postgres" {
			t.Skip("only PostgreSQL keeps slots an ordinary account cannot use")
		}
		clean(t)
		tuner, sqlDB := poolTuner(t, host)
		var maxConns, reserved int
		require.NoError(t, sqlDB.QueryRow(`SELECT
			(SELECT setting::int FROM pg_settings WHERE name = 'max_connections'),
			(SELECT COALESCE(sum(setting::int), 0) FROM pg_settings
				WHERE name IN ('superuser_reserved_connections', 'reserved_connections'))`).Scan(&maxConns, &reserved))
		size := int(float64(maxConns-reserved)*0.34) + 1
		if float64(size) > float64(maxConns)*0.34 {
			t.Skipf("%d reserved of %d leaves no pool size between the two thirds", reserved, maxConns)
		}

		view, err := tuner.SetDatabaseMaxConns(size)
		require.NoError(t, err)
		assert.Contains(t, view.Warning, fmt.Sprintf("of the %d the database accepts", maxConns-reserved))
	})
}
