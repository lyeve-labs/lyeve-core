package provider

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// engineFromDSN: string -> engine selection

func TestEngineFromDSN_Selection(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"postgres url scheme", "postgresql://u:p@localhost:5432/db", "postgres"},
		{"postgres short scheme", "postgres://u:p@localhost:5432/db", "postgres"},
		{"postgres keyword=value fallback", "host=localhost dbname=db user=postgres", "postgres"},
		{"mysql url scheme", "mysql://u:p@localhost:3306/db", "mysql"},
		{"mysql tcp dsn", "u:p@tcp(localhost:3306)/db", "mysql"},
		{"mssql sqlserver scheme", "sqlserver://u:p@localhost:1433?database=db", "mssql"},
		{"mssql mssql scheme", "mssql://u:p@localhost:1433?database=db", "mssql"},
		{"unknown scheme falls back to postgres", "oracle://u:p@host:1521/db", "postgres"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Registry.engineFromDSN(tt.dsn))
		})
	}
}

func TestRegistry_Register_DuplicatePanics(t *testing.T) {
	r := &registry{providers: make(map[string]Provider)}
	p := Registry.MustGet("postgres")

	r.Register(p) // first registration succeeds
	assert.Equal(t, p, r.Get("postgres"))

	assert.PanicsWithValue(t,
		`provider: duplicate registration for "postgres"`,
		func() { r.Register(p) },
	)
}

// DSNInfo.String / mapPairs

func TestDSNInfo_String_RedactsPassword(t *testing.T) {
	info := DSNInfo{
		Engine:   "postgres",
		Host:     "localhost",
		Port:     5432,
		DBName:   "mydb",
		User:     "u",
		Password: "s3cret",
		Params:   map[string]string{"sslmode": "disable"},
	}
	got := info.String()
	assert.Equal(t,
		"engine=postgres host=localhost port=5432 dbname=mydb user=u password=*** params=sslmode=disable",
		got,
	)
	assert.NotContains(t, got, "s3cret", "raw password must never appear")
}

func TestDSNInfo_String_EmptyPasswordAndParams(t *testing.T) {
	got := DSNInfo{}.String()
	// Empty password renders as "password=" (no ***); nil params render empty.
	assert.Equal(t, "engine= host= port=0 dbname= user= password= params=", got)
}

// Capabilities.String / Capability.Name edge cases

func TestCapabilities_String_None(t *testing.T) {
	assert.Equal(t, "(none)", NewCapabilities().String())
}

func TestCapability_Name(t *testing.T) {
	assert.Equal(t, "(zero)", Capability(0).Name())
	assert.Equal(t, "CreateTableIfNotExists", CapCreateTableIfNotExists.Name())
	assert.Equal(t, "Upsert:Merge", CapUpsertMerge.Name())
	// A combined (non-single-bit) value has no name -> numeric fallback.
	combined := CapCTE | CapWindowFunc // 8 | 32 = 40
	assert.Equal(t, "Capability(40)", combined.Name())
}

// DefaultPoolOptions

func TestDefaultPoolOptions(t *testing.T) {
	opts := DefaultPoolOptions()
	assert.Equal(t, int32(25), opts.MaxConns)
	assert.Equal(t, int32(2), opts.MinConns)
	assert.Equal(t, 1*time.Hour, opts.ConnMaxLifetime)
	assert.Equal(t, 5*time.Minute, opts.ConnMaxIdleTime)
	assert.Equal(t, 30*time.Second, opts.HealthCheckPeriod)
}

// DSNInfo.String / mapPairs

func TestDSNParser_MySQL_HostWithoutPort(t *testing.T) {
	info := mysqlDSNParser{}.Parse("user:pass@tcp(localhost)/appdb")
	assert.Equal(t, "mysql", info.Engine)
	assert.Equal(t, "localhost", info.Host)
	assert.Equal(t, 0, info.Port) // no port specified
	assert.Equal(t, "appdb", info.DBName)
	assert.Equal(t, "user", info.User)
	assert.Equal(t, "pass", info.Password)
}

func TestDSNParser_MSSQL_NoUserNoPort(t *testing.T) {
	info := mssqlDSNParser{}.Parse("sqlserver://dbhost?database=appdb")
	assert.Equal(t, "mssql", info.Engine)
	assert.Equal(t, "dbhost", info.Host)
	assert.Equal(t, 0, info.Port)
	assert.Equal(t, "", info.User)
	assert.Equal(t, "appdb", info.DBName)
}

func TestDSNParser_MSSQL_UnparseableURL(t *testing.T) {
	// A raw control character makes url.Parse fail. Parser returns the
	// zero-valued info with only Engine and Raw populated.
	raw := "sqlserver://\x7f"
	info := mssqlDSNParser{}.Parse(raw)
	assert.Equal(t, "mssql", info.Engine)
	assert.Equal(t, raw, info.Raw)
	assert.Empty(t, info.Host)
	assert.Nil(t, info.Params)
}

// FailoverDB (driven with in-memory db.DB fakes, no live backend)

// recordingDB is an in-memory db.DB fake. Ping/Close errors are configurable
// (to drive the health-check state machine), and read/write method calls are
// counted so tests can assert which pool a query was routed to.
type recordingDB struct {
	engine   string
	pingErr  error
	closeErr error
	stats    sql.DBStats

	queryRowN, queryN, execN, beginN, connN, querierRON int
}

func (r *recordingDB) QueryRow(_ context.Context, _ string, _ ...any) (*sql.Row, error) {
	r.queryRowN++
	return nil, nil
}
func (r *recordingDB) Query(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	r.queryN++
	return nil, nil
}
func (r *recordingDB) Exec(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	r.execN++
	return nil, nil
}
func (r *recordingDB) Begin(_ context.Context) (*sql.Tx, error) {
	r.beginN++
	return nil, nil
}
func (r *recordingDB) Conn(_ context.Context) (*sql.Conn, error) {
	r.connN++
	return nil, nil
}
func (r *recordingDB) Ping(_ context.Context) error { return r.pingErr }
func (r *recordingDB) Close() error                 { return r.closeErr }
func (r *recordingDB) Stats() sql.DBStats           { return r.stats }
func (r *recordingDB) Engine() string               { return r.engine }
func (r *recordingDB) SQLDB() *sql.DB               { return nil }
func (r *recordingDB) QuerierRO(_ context.Context) (db.ReadOnlyQuerier, error) {
	r.querierRON++
	return nil, nil
}

func TestNewFailoverDB_Defaults(t *testing.T) {
	primary := &recordingDB{engine: "postgres"}
	rep := &recordingDB{engine: "postgres"}

	f := NewFailoverDB(FailoverConfig{Primary: primary, Replicas: []db.DB{rep}})
	// Non-positive CheckInterval defaults to 5s.
	assert.Equal(t, 5*time.Second, f.checkInterval)
	// Active starts on the primary. Both pools start healthy.
	assert.Equal(t, db.DB(primary), f.activeForReads())
	assert.True(t, f.healthy[primary])
	assert.True(t, f.healthy[rep])

	// Explicit interval is preserved.
	f2 := NewFailoverDB(FailoverConfig{Primary: primary, CheckInterval: 250 * time.Millisecond})
	assert.Equal(t, 250*time.Millisecond, f2.checkInterval)
}

func TestFailoverDB_Routing(t *testing.T) {
	primary := &recordingDB{engine: "postgres", stats: sql.DBStats{MaxOpenConnections: 7}}
	rep := &recordingDB{engine: "postgres"}
	f := NewFailoverDB(FailoverConfig{Primary: primary, Replicas: []db.DB{rep}})
	ctx := context.Background()

	// Reads route to the active pool (initially primary).
	f.QueryRow(ctx, "SELECT 1")
	_, _ = f.Query(ctx, "SELECT 1")
	_, _ = f.Conn(ctx)
	_, _ = f.QuerierRO(ctx)
	assert.Equal(t, 1, primary.queryRowN)
	assert.Equal(t, 1, primary.queryN)
	assert.Equal(t, 1, primary.connN)
	assert.Equal(t, 1, primary.querierRON)

	// Writes always route to the primary.
	_, _ = f.Exec(ctx, "INSERT")
	_, _ = f.Begin(ctx)
	assert.Equal(t, 1, primary.execN)
	assert.Equal(t, 1, primary.beginN)

	// Replica saw nothing while the primary is healthy.
	assert.Zero(t, rep.queryRowN+rep.queryN+rep.execN+rep.beginN)

	// Ping/Engine/Stats/SQLDB delegate to the active pool.
	primary.pingErr = errors.New("boom")
	assert.EqualError(t, f.Ping(ctx), "boom")
	assert.Equal(t, "postgres", f.Engine())
	assert.Equal(t, 7, f.Stats().MaxOpenConnections)
	assert.Nil(t, f.SQLDB())
}

func TestFailoverDB_HealthCheck_SwitchAndRecover(t *testing.T) {
	primary := &recordingDB{engine: "postgres"}
	rep := &recordingDB{engine: "postgres"}
	f := NewFailoverDB(FailoverConfig{Primary: primary, Replicas: []db.DB{rep}})
	ctx := context.Background()

	// Primary goes down -> reads switch to the healthy replica.
	primary.pingErr = errors.New("primary down")
	f.runHealthCheck(ctx)
	assert.Equal(t, db.DB(rep), f.activeForReads())
	assert.False(t, f.healthy[primary])

	// A read now lands on the replica.
	f.QueryRow(ctx, "SELECT 1")
	assert.Equal(t, 1, rep.queryRowN)

	// Primary recovers -> reads switch back to the primary.
	primary.pingErr = nil
	f.runHealthCheck(ctx)
	assert.Equal(t, db.DB(primary), f.activeForReads())
	assert.True(t, f.healthy[primary])
}

func TestFailoverDB_HealthCheck_NoHealthyReplicaStaysOnPrimary(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("primary down")}
	rep := &recordingDB{engine: "postgres", pingErr: errors.New("replica down")}
	f := NewFailoverDB(FailoverConfig{Primary: primary, Replicas: []db.DB{rep}})

	f.runHealthCheck(context.Background())

	// With no healthy replica, reads stay on the (unhealthy) primary rather
	// than pointing at a nil/absent pool.
	assert.Equal(t, db.DB(primary), f.activeForReads())
	assert.False(t, f.healthy[primary])
	assert.False(t, f.healthy[rep])
}

func TestFailoverDB_Close_AggregatesErrors(t *testing.T) {
	primary := &recordingDB{closeErr: errors.New("primary boom")}
	rep := &recordingDB{closeErr: errors.New("replica boom")}
	f := NewFailoverDB(FailoverConfig{Primary: primary, Replicas: []db.DB{rep}})

	err := f.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "primary")
	assert.Contains(t, err.Error(), "replica[0]")

	// All-clean close returns nil.
	f2 := NewFailoverDB(FailoverConfig{Primary: &recordingDB{}})
	assert.NoError(t, f2.Close())
}

func TestFailoverDB_StartIdempotent_AndStop(t *testing.T) {
	// Long interval so the background poller never fires a tick during the
	// test: Stop() (via stopCh) is what terminates the goroutine.
	f := NewFailoverDB(FailoverConfig{Primary: &recordingDB{}, CheckInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	f.Start(ctx)
	assert.True(t, f.started)
	f.Start(ctx) // second call is a no-op (already started)
	f.Stop()
	f.Stop() // and Stop is safe to repeat

	// Assert the goroutine actually left rather than trusting that it did. A
	// leaked health loop keeps pinging a pool the caller has closed.
	select {
	case <-f.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("the health loop did not exit after Stop")
	}
}

// The breaker opens only after the configured number of consecutive failures,
// not on the first one: a single slow moment must not refuse writes.
func TestFailoverCircuit_OpensOnlyAfterConsecutiveFailures(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("down")}
	f := NewFailoverDB(FailoverConfig{
		Primary:             primary,
		CheckInterval:       time.Hour, // no background loop interferes
		MaxConsecutiveFails: 3,
		CircuitCooldown:     time.Hour,
	})
	ctx := context.Background()

	for i := 1; i < 3; i++ {
		f.runHealthCheck(ctx)
		require.False(t, f.CircuitOpen(), "opened after %d of 3 failures", i)
		_, err := f.Exec(ctx, "INSERT")
		assert.NoError(t, err, "writes must still be admitted below the threshold")
	}

	f.runHealthCheck(ctx)
	require.True(t, f.CircuitOpen(), "the third consecutive failure must open it")

	_, err := f.Exec(ctx, "INSERT")
	require.ErrorIs(t, err, ErrCircuitOpen)
	_, err = f.Begin(ctx)
	require.ErrorIs(t, err, ErrCircuitOpen,
		"Begin is refused too, so nobody holds a transaction they cannot commit")
}

// A run of failures that is broken by a success starts counting again.
func TestFailoverCircuit_OneGoodCheckResetsTheCount(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("down")}
	f := NewFailoverDB(FailoverConfig{
		Primary: primary, CheckInterval: time.Hour, MaxConsecutiveFails: 3,
	})
	ctx := context.Background()

	f.runHealthCheck(ctx)
	f.runHealthCheck(ctx)
	primary.pingErr = nil
	f.runHealthCheck(ctx) // the run is broken here
	primary.pingErr = errors.New("down again")
	f.runHealthCheck(ctx)
	f.runHealthCheck(ctx)

	assert.False(t, f.CircuitOpen(),
		"two failures after a success is not three in a row")
}

// The primary answering closes the breaker, without waiting for the cooldown.
func TestFailoverCircuit_RecoveryClosesItImmediately(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("down")}
	f := NewFailoverDB(FailoverConfig{
		Primary: primary, CheckInterval: time.Hour,
		MaxConsecutiveFails: 1, CircuitCooldown: time.Hour,
	})
	ctx := context.Background()

	f.runHealthCheck(ctx)
	require.True(t, f.CircuitOpen())

	primary.pingErr = nil
	f.runHealthCheck(ctx)
	require.False(t, f.CircuitOpen(), "a healthy primary must not wait out the cooldown")

	_, err := f.Exec(ctx, "INSERT")
	assert.NoError(t, err)
}

// Once the cooldown elapses writes are admitted again without a health check,
// so a breaker cannot outlive a stopped health loop.
func TestFailoverCircuit_CooldownAdmitsWritesWithoutTheHealthLoop(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("down")}
	f := NewFailoverDB(FailoverConfig{
		Primary: primary, CheckInterval: time.Hour,
		MaxConsecutiveFails: 1, CircuitCooldown: 30 * time.Millisecond,
	})
	ctx := context.Background()

	f.runHealthCheck(ctx)
	require.True(t, f.CircuitOpen())
	_, err := f.Exec(ctx, "INSERT")
	require.ErrorIs(t, err, ErrCircuitOpen)

	time.Sleep(40 * time.Millisecond)

	_, err = f.Exec(ctx, "INSERT")
	assert.NoError(t, err, "the cooldown elapsed, so the write is admitted")
	assert.False(t, f.CircuitOpen())
}

// The refusal says how much longer, so an operator reading one log line knows
// whether to wait or to look.
func TestFailoverCircuit_RefusalNamesTheRemainingCooldown(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("down")}
	f := NewFailoverDB(FailoverConfig{
		Primary: primary, CheckInterval: time.Hour,
		MaxConsecutiveFails: 1, CircuitCooldown: time.Minute,
	})
	f.runHealthCheck(context.Background())

	_, err := f.Exec(context.Background(), "INSERT")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writes are refused")
	assert.Contains(t, err.Error(), "for another")
}

// Concurrent writers crossing the cooldown boundary must not corrupt the
// breaker's state. Reading the state under a read lock and then taking a write
// lock to reset would let two callers both reset.
func TestFailoverCircuit_ConcurrentWritersAcrossTheCooldown(t *testing.T) {
	// recordingDB counts with plain ints and is only safe for one goroutine,
	// so this case brings its own pool rather than making every other test pay
	// for atomics it does not need.
	primary := &concurrentDB{pingErr: errors.New("down")}
	f := NewFailoverDB(FailoverConfig{
		Primary: primary, CheckInterval: time.Hour,
		MaxConsecutiveFails: 1, CircuitCooldown: 20 * time.Millisecond,
	})
	ctx := context.Background()
	f.runHealthCheck(ctx)
	require.True(t, f.CircuitOpen())

	var wg sync.WaitGroup
	var refused, admitted atomic.Int64
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(rand.IntN(40)) * time.Millisecond)
			if _, err := f.Exec(ctx, "INSERT"); err != nil {
				refused.Add(1)
			} else {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Positive(t, admitted.Load(), "every writer past the cooldown must get through")
	assert.False(t, f.CircuitOpen(), "the breaker is closed once the cooldown has passed")
	assert.EqualValues(t, 64, refused.Load()+admitted.Load())
}

// An observer sees the pool change and may call back in without deadlocking,
// which is only true while it is invoked outside the lock.
func TestFailoverCircuit_ObserverSeesTheSwitchAndMayCallBack(t *testing.T) {
	primary := &recordingDB{engine: "postgres", pingErr: errors.New("down")}
	rep := &recordingDB{engine: "postgres"}

	var mu sync.Mutex
	var seen []string
	var open bool
	// Declared before the config so the observer can close over it: calling
	// back into the type under observation is the property being tested.
	var f *FailoverDB
	f = NewFailoverDB(FailoverConfig{
		Primary: primary, Replicas: []db.DB{rep}, CheckInterval: time.Hour,
		OnStateChange: func(from, to db.DB, reason string) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, reason)
			open = openOf(f) // calls back into the type under observation
		},
	})

	f.runHealthCheck(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"health-check"}, seen)
	assert.False(t, open, "one failure does not open the default breaker")
	assert.Equal(t, db.DB(rep), f.activeForReads())
}

// openOf exists so the observer's call back into the type is a real one rather
// than a field read the compiler could reorder away.
func openOf(f *FailoverDB) bool { return f.CircuitOpen() }

// No switch means no observer call: a callback on every tick would be noise an
// operator learns to ignore.
func TestFailoverCircuit_ObserverIsSilentWhileNothingChanges(t *testing.T) {
	primary := &recordingDB{engine: "postgres"}
	calls := 0
	f := NewFailoverDB(FailoverConfig{
		Primary: primary, CheckInterval: time.Hour,
		OnStateChange: func(db.DB, db.DB, string) { calls++ },
	})
	for i := 0; i < 5; i++ {
		f.runHealthCheck(context.Background())
	}
	assert.Zero(t, calls)
}

// Defaults are the ones FailoverConfig documents.
func TestFailoverCircuit_Defaults(t *testing.T) {
	f := NewFailoverDB(FailoverConfig{Primary: &recordingDB{}})
	assert.Equal(t, defaultMaxConsecutiveFails, f.maxConsecutiveFails)
	assert.Equal(t, defaultCircuitCooldown, f.circuitCooldown)
	assert.False(t, f.CircuitOpen(), "a new wrapper admits writes")
}

// concurrentDB is a db.DB several goroutines may call at once. It counts
// nothing the other fakes count. The concurrency test asserts on the breaker,
// not on the pool.
type concurrentDB struct {
	pingErr error
	execs   atomic.Int64
}

func (c *concurrentDB) QueryRow(context.Context, string, ...any) (*sql.Row, error) {
	return nil, nil
}
func (c *concurrentDB) Query(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}
func (c *concurrentDB) Exec(context.Context, string, ...any) (sql.Result, error) {
	c.execs.Add(1)
	return nil, nil
}
func (c *concurrentDB) Begin(context.Context) (*sql.Tx, error)  { return nil, nil }
func (c *concurrentDB) Conn(context.Context) (*sql.Conn, error) { return nil, nil }
func (c *concurrentDB) Ping(context.Context) error              { return c.pingErr }
func (c *concurrentDB) Close() error                            { return nil }
func (c *concurrentDB) Stats() sql.DBStats                      { return sql.DBStats{} }
func (c *concurrentDB) Engine() string                          { return "postgres" }
func (c *concurrentDB) SQLDB() *sql.DB                          { return nil }
func (c *concurrentDB) QuerierRO(context.Context) (db.ReadOnlyQuerier, error) {
	return nil, nil
}
