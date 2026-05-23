package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

// FailoverDB wraps a primary db.DB with replica fallback. When the primary
// becomes unhealthy (Ping fails), reads (QueryRow, Query) transparently
// route to the next healthy replica. Writes (Exec, Begin) always go to
// the primary: the caller gets an error if the primary is dead.
//
// A background goroutine polls the primary until it recovers, then
// switches reads back automatically.
type FailoverDB struct {
	primary  db.DB
	replicas []db.DB
	mu       sync.RWMutex
	active   db.DB          // currently active for reads (primary or replica)
	healthy  map[db.DB]bool // track health per pool

	checkInterval time.Duration
	stopCh        chan struct{}
	stopOnce      sync.Once
	started       bool

	// doneCh closes when the health loop returns, so Stop can be observed to
	// have stopped it rather than assumed to have.
	doneCh chan struct{}

	// Circuit breaker. A primary that fails its health check repeatedly is
	// almost certainly not going to accept a write either, so writes are
	// refused outright rather than queued against it until they time out.
	// Every field here is read and written under mu.
	consecutiveFails    int
	maxConsecutiveFails int
	circuitOpen         bool
	openedAt            time.Time
	circuitCooldown     time.Duration

	// onStateChange reports a change of the pool serving reads. Called
	// without mu held, so an observer may call back into this type.
	onStateChange func(from, to db.DB, reason string)
}

// FailoverConfig configures the failover wrapper.
type FailoverConfig struct {
	Primary       db.DB
	Replicas      []db.DB
	CheckInterval time.Duration

	// MaxConsecutiveFails is how many health checks in a row the primary may
	// fail before writes are refused. Zero takes the default.
	//
	// The default of 3 against the default 5s interval means roughly fifteen
	// seconds of a primary failing to answer a 2s ping before a write is
	// refused. That is long enough that a slow moment does not trip it and
	// short enough that a caller is not left waiting on a pool that is gone.
	MaxConsecutiveFails int

	// CircuitCooldown is how long writes stay refused once the breaker opens.
	// Zero takes the default.
	CircuitCooldown time.Duration

	// OnStateChange, when set, is called whenever the pool serving reads
	// changes, with the pool it moved from, the pool it moved to, and why.
	OnStateChange func(from, to db.DB, reason string)
}

const (
	// defaultMaxConsecutiveFails and defaultCircuitCooldown are the settings a
	// caller gets without asking. See FailoverConfig for the arithmetic.
	defaultMaxConsecutiveFails = 3
	defaultCircuitCooldown     = 30 * time.Second
)

// NewFailoverDB wraps a primary and replica pools into a failover-aware DB.
func NewFailoverDB(cfg FailoverConfig) *FailoverDB {
	if cfg.CheckInterval <= 0 {
		cfg.CheckInterval = 5 * time.Second
	}
	if cfg.MaxConsecutiveFails <= 0 {
		cfg.MaxConsecutiveFails = defaultMaxConsecutiveFails
	}
	if cfg.CircuitCooldown <= 0 {
		cfg.CircuitCooldown = defaultCircuitCooldown
	}
	healthy := map[db.DB]bool{cfg.Primary: true}
	for _, r := range cfg.Replicas {
		healthy[r] = true
	}
	return &FailoverDB{
		primary:       cfg.Primary,
		replicas:      cfg.Replicas,
		active:        cfg.Primary,
		healthy:       healthy,
		checkInterval: cfg.CheckInterval,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),

		maxConsecutiveFails: cfg.MaxConsecutiveFails,
		circuitCooldown:     cfg.CircuitCooldown,
		onStateChange:       cfg.OnStateChange,
	}
}

// Start begins background health polling. Call once after construction.
func (f *FailoverDB) Start(ctx context.Context) {
	f.mu.Lock()
	if f.started {
		f.mu.Unlock()
		return
	}
	f.started = true
	f.mu.Unlock()
	go f.healthLoop(ctx)
}

// Stop terminates the background health poller. Safe to call more than once.
func (f *FailoverDB) Stop() {
	f.stopOnce.Do(func() {
		close(f.stopCh)
	})
}

// activeForReads returns the pool to use for read queries.
func (f *FailoverDB) activeForReads() db.DB {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.active
}

// activePrimary returns the primary pool.
func (f *FailoverDB) activePrimary() db.DB {
	return f.primary
}

// db.DB implementation for FailoverDB.

// QueryRow executes a read query on the currently active pool (primary or
// healthy replica). Reads route through the replica when the primary is down.
func (f *FailoverDB) QueryRow(ctx context.Context, q string, args ...any) (*sql.Row, error) {
	return f.activeForReads().QueryRow(ctx, q, args...)
}

// Query executes a read query on the currently active pool (primary or
// healthy replica). Returns an error when all pools are unhealthy.
func (f *FailoverDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return f.activeForReads().Query(ctx, q, args...)
}

// Exec executes a write query on the primary pool. Returns an error when the
// primary is unavailable (writes never route through replicas) and refuses
// outright while the circuit breaker is open.
func (f *FailoverDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if err := f.admitWrite(); err != nil {
		return nil, err
	}
	return f.activePrimary().Exec(ctx, q, args...)
}

// Begin starts a transaction on the primary pool, and refuses while the
// circuit breaker is open. Refusing here rather than at the first statement
// means a caller never holds a transaction it cannot commit.
func (f *FailoverDB) Begin(ctx context.Context) (*sql.Tx, error) {
	if err := f.admitWrite(); err != nil {
		return nil, err
	}
	return f.activePrimary().Begin(ctx)
}

// ErrCircuitOpen is returned by a write while the breaker is open. Callers
// that distinguish "the database refused this" from "the database is not
// taking writes right now" match on it.
var ErrCircuitOpen = errors.New("primary is failing its health check, writes are refused")

// admitWrite reports whether a write may proceed.
//
// The whole decision, including the reset, happens under one write lock. The
// obvious shape: read the state under RLock, then take the lock to reset -
// lets two callers each observe an elapsed cooldown and each reset, which is
// the sort of race that only shows up under the load the breaker exists for.
//
// Once the cooldown elapses the breaker closes and writes are admitted again.
// It does not admit a single probe and hold the rest: that would need the
// probe's outcome reported back, and a breaker left half-open by a caller that
// never returned would refuse every write until the process restarted. The
// health loop re-opens it on the next tick if the primary is still failing,
// which costs one interval and cannot wedge.
func (f *FailoverDB) admitWrite() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.circuitOpen {
		return nil
	}
	if time.Since(f.openedAt) >= f.circuitCooldown {
		f.circuitOpen = false
		f.consecutiveFails = 0
		slog.Info("failover: circuit breaker cooled down, writes admitted again",
			"cooldown", f.circuitCooldown.String())
		return nil
	}
	return fmt.Errorf("%w (for another %s)", ErrCircuitOpen,
		(f.circuitCooldown - time.Since(f.openedAt)).Round(time.Second))
}

// CircuitOpen reports whether writes are currently being refused.
func (f *FailoverDB) CircuitOpen() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.circuitOpen
}

// Conn returns a dedicated connection from the active pool for reads.
func (f *FailoverDB) Conn(ctx context.Context) (*sql.Conn, error) {
	return f.activeForReads().Conn(ctx)
}

// Ping verifies connectivity through the currently active pool.
func (f *FailoverDB) Ping(ctx context.Context) error {
	return f.activeForReads().Ping(ctx)
}

// Close shuts down the primary and all replica pools, collecting any errors.
func (f *FailoverDB) Close() error {
	var errs []error
	if err := f.primary.Close(); err != nil {
		errs = append(errs, fmt.Errorf("primary: %w", err))
	}
	for i, r := range f.replicas {
		if err := r.Close(); err != nil {
			errs = append(errs, fmt.Errorf("replica[%d]: %w", i, err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("failover close errors: %v", errs)
}

// Stats returns pool statistics from the currently active pool.
func (f *FailoverDB) Stats() sql.DBStats {
	return f.activeForReads().Stats()
}

// Engine returns the database engine name from the primary pool.
func (f *FailoverDB) Engine() string {
	return f.primary.Engine()
}

// SQLDB returns the underlying *sql.DB from the active pool.
func (f *FailoverDB) SQLDB() *sql.DB {
	return f.activeForReads().SQLDB()
}

// DatabaseName forwards the primary's own database name, the same pool Engine
// reports for. The tenancy strategy is built once and re-pins to a fixed name,
// so it has to be the primary's rather than whichever pool happens to be
// serving reads at the moment.
func (f *FailoverDB) DatabaseName() string {
	return db.DatabaseNameOf(f.activePrimary())
}

// QuerierRO returns a read-only querier from the active pool.
func (f *FailoverDB) QuerierRO(ctx context.Context) (db.ReadOnlyQuerier, error) {
	return f.activeForReads().QuerierRO(ctx)
}

// Health loop.

func (f *FailoverDB) healthLoop(ctx context.Context) {
	defer close(f.doneCh)
	ticker := time.NewTicker(f.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stopCh:
			return
		case <-ticker.C:
			f.runHealthCheck(ctx)
		}
	}
}

func (f *FailoverDB) runHealthCheck(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// The observer runs after the lock is released, so it may call back into
	// this type without deadlocking. The locked half is its own function so
	// the unlock is deferred and survives a panic from a pool's Ping.
	if notify := f.check(ctx, pingCtx); notify != nil {
		notify()
	}
}

// check runs one health pass and returns the observer call the caller owes, or
// nil. Everything it touches is under mu.
func (f *FailoverDB) check(ctx, pingCtx context.Context) (notify func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prevActive := f.active

	// Check primary
	if err := f.primary.Ping(pingCtx); err != nil {
		f.consecutiveFails++
		if f.consecutiveFails >= f.maxConsecutiveFails && !f.circuitOpen {
			f.circuitOpen = true
			f.openedAt = time.Now()
			slog.Error("failover: primary failed its health check repeatedly, refusing writes",
				"consecutive_fails", f.consecutiveFails,
				"cooldown", f.circuitCooldown.String(), "err", err)
		}
		if f.healthy[f.primary] {
			slog.Warn("failover: primary unhealthy, switching reads to replica",
				"err", err)
			f.healthy[f.primary] = false
			f.switchToReplica(ctx)
		}
	} else {
		if f.consecutiveFails > 0 || f.circuitOpen {
			slog.Info("failover: primary answered, writes admitted again",
				"was_open", f.circuitOpen, "after_fails", f.consecutiveFails)
			f.consecutiveFails = 0
			f.circuitOpen = false
		}
		if !f.healthy[f.primary] {
			slog.Info("failover: primary healthy, switching reads back")
			f.healthy[f.primary] = true
			f.active = f.primary
		}
	}

	// Check replicas
	for _, r := range f.replicas {
		if err := r.Ping(pingCtx); err != nil {
			f.healthy[r] = false
		} else {
			f.healthy[r] = true
		}
	}

	// If current active is unhealthy, try to switch
	if !f.healthy[f.active] {
		f.switchToReplica(ctx)
	}

	if f.onStateChange != nil && prevActive != f.active {
		from, to, observer := prevActive, f.active, f.onStateChange
		return func() { observer(from, to, "health-check") }
	}
	return nil
}

func (f *FailoverDB) switchToReplica(ctx context.Context) {
	for _, r := range f.replicas {
		if f.healthy[r] {
			slog.Info("failover: switched reads to replica")
			f.active = r
			return
		}
	}
	// No healthy replica found: keep primary as active even though it's
	// unhealthy. Callers will get errors from their queries, which is better
	// than nil-pointer panics.
	f.active = f.primary
	slog.Error("failover: no healthy replica available, reads stay on primary")
}
