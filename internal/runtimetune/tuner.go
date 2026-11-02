package runtimetune

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// automaticRatio is the share of the memory limit the soft limit is set to
// when nothing else sets it. It leaves 15% for memory the Go runtime does not
// count, such as cgo and the kernel's buffers for the process.
const automaticRatio = 0.85

// crowdingCheckLimit bounds the query a pool resize makes to read the
// server's connection limit.
const crowdingCheckLimit = 5 * time.Second

// Replicas is what the tuner reads about the other replicas. The engine's
// relay implements it.
type Replicas interface {
	Replicas(ctx context.Context) (n int, known bool)
	Attached() bool
}

// Options is what a Tuner reads.
type Options struct {
	// Root is the file system the cgroup files and /proc are read under:
	// os.DirFS("/") in the engine.
	Root fs.FS
	// DB is the primary pool, or nil on an engine without a database.
	DB *sql.DB
	// Dialect names DB's database.
	Dialect string
	// Config holds the values the engine started with.
	Config *config.Config
	// Resolver answers where a configuration key came from. Nil reads the
	// active resolver on each call.
	Resolver *config.Resolver
	// Replicas counts the replicas. Nil reports this one alone.
	Replicas Replicas
	// StartProcs is the thread count the engine passed to
	// runtime.GOMAXPROCS at start, or 0 when it left Go's default.
	StartProcs int
}

// Tuner implements core.RuntimeTuning over the process's runtime and its
// primary pool. Everything it changes is process-wide, so the engine builds
// one.
type Tuner struct {
	root     fs.FS
	db       *sql.DB
	dialect  string
	cfg      *config.Config
	resolver *config.Resolver
	replicas Replicas
	hostCPUs int

	mu sync.Mutex

	// cpuStartVar names the variable the thread count started from, until a
	// write replaces it. cpuMode and cpuThreads are the last write's.
	// cpuOne is set while the automatic mode holds one thread under a CPU
	// limit of one core or less.
	cpuStartVar string
	cpuMode     string
	cpuThreads  int
	cpuOne      bool

	// memStartVar names the variable the soft limit started from, until a
	// write replaces it. mem is the last write, or automatic. memApplied is
	// the limit this tuner or the start last set, and memBase where the
	// limit it is a share of came from.
	memStartVar string
	mem         core.MemoryLimit
	memApplied  int64
	memBase     string

	// gcSet is the percentage the last write set, or nil before one.
	gcSet *int
	// dbSet is the pool size the last write set, or 0 for the start size.
	dbSet int

	// sharedCache is 0 until the engine has chosen its cache backend, then
	// 1 for one held in this process and 2 for one every replica shares.
	sharedCache atomic.Int32
}

var _ core.RuntimeTuning = (*Tuner)(nil)

// New returns a tuner over the state the engine started with, which the
// engine set before calling it: the thread count in opts.StartProcs and the
// soft limit from the configured memory limit.
func New(opts Options) *Tuner {
	if opts.Root == nil {
		opts.Root = os.DirFS("/")
	}
	if opts.Config == nil {
		opts.Config = &config.Config{}
	}
	t := &Tuner{
		root:     opts.Root,
		db:       opts.DB,
		dialect:  opts.Dialect,
		cfg:      opts.Config,
		resolver: opts.Resolver,
		replicas: opts.Replicas,
		hostCPUs: runtime.NumCPU(),
		cpuMode:  core.CPUAutomatic,
		mem:      core.MemoryLimit{Mode: core.MemoryAutomatic},
	}
	t.cpuStartVar, _ = t.cpuVariable()
	// The start's own count, not the limit read again: a limit that changed
	// since then would leave one thread held with nothing to hand it back.
	t.cpuOne = t.cpuStartVar == "" && opts.StartProcs == 1
	t.memStartVar, _ = t.memoryVariable()
	t.memApplied = debug.SetMemoryLimit(-1)
	_, t.memBase = t.memoryLimit()
	return t
}

// EngineProcs is the engine's own rule for the scheduler's thread count when
// nothing sets one: 1 under a CPU limit of one core or less on a machine with
// two or more CPUs, where Go's default would give 2, and 0 otherwise to leave
// Go's default and its live updates in place. Measured under a one-core
// limit, two threads served no better than one.
func EngineProcs(cpuLimit float64, hostCPUs int) int {
	if cpuLimit > 0 && cpuLimit <= 1 && hostCPUs >= 2 {
		return 1
	}
	return 0
}

// SetSharedCache records whether the cache backend the engine chose is shared
// by every replica. The engine chooses it after the plugins start.
func (t *Tuner) SetSharedCache(shared bool) {
	if shared {
		t.sharedCache.Store(2)
	} else {
		t.sharedCache.Store(1)
	}
}

// Tick reads the CPU and memory limits again and applies a change to them:
// the thread count while it is automatic, and the soft limit while it is
// automatic or a ratio. A value pinned by the deployment or fixed by a write
// does not follow. The runtime is called only when a value changes.
func (t *Tuner) Tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cpuStartVar == "" && t.cpuMode == core.CPUAutomatic {
		t.applyAutomaticCPU(config.CgroupCPULimit(t.root))
	}
	if t.memStartVar == "" && (t.mem.Mode == core.MemoryAutomatic || t.mem.Mode == core.MemoryRatio) {
		limit, base := t.memoryLimit()
		if limit > 0 {
			t.memBase = base
			if target := shareOf(limit, t.ratio()); target != t.memApplied {
				debug.SetMemoryLimit(target)
				t.memApplied = target
			}
		}
	}
}

// applyAutomaticCPU holds one thread under a limit of one core or less, and
// hands the count back to Go's default when the limit grows past it.
func (t *Tuner) applyAutomaticCPU(cpuLimit float64) {
	one := EngineProcs(cpuLimit, t.hostCPUs) == 1
	switch {
	case one && !t.cpuOne:
		runtime.GOMAXPROCS(1)
	case !one && t.cpuOne:
		runtime.SetDefaultGOMAXPROCS()
	}
	t.cpuOne = one
}

// SetCPULimit implements core.RuntimeTuning.
func (t *Tuner) SetCPULimit(threads int) (core.RuntimeSettingsView, error) {
	if variable, pinned := t.cpuVariable(); pinned {
		return core.RuntimeSettingsView{}, &core.PinnedError{Variable: variable}
	}
	if threads < 0 || threads > t.hostCPUs {
		return core.RuntimeSettingsView{}, fmt.Errorf("CPU threads must be between 1 and %d, or 0 for automatic", t.hostCPUs)
	}
	t.mu.Lock()
	t.cpuStartVar = ""
	if threads == 0 {
		t.cpuMode = core.CPUAutomatic
		// SetDefaultGOMAXPROCS first, so a count fixed before returns to
		// Go's default even where the engine's rule then holds one thread.
		runtime.SetDefaultGOMAXPROCS()
		t.cpuOne = false
		t.applyAutomaticCPU(config.CgroupCPULimit(t.root))
	} else {
		t.cpuMode = core.CPUFixed
		t.cpuThreads = threads
		t.cpuOne = false
		runtime.GOMAXPROCS(threads)
	}
	t.mu.Unlock()
	return t.RuntimeSettings(), nil
}

// SetMemoryLimit implements core.RuntimeTuning.
func (t *Tuner) SetMemoryLimit(m core.MemoryLimit) (core.RuntimeSettingsView, error) {
	if variable, pinned := t.memoryVariable(); pinned {
		return core.RuntimeSettingsView{}, &core.PinnedError{Variable: variable}
	}
	t.mu.Lock()
	target, base, err := t.memoryTarget(m)
	if err != nil {
		t.mu.Unlock()
		return core.RuntimeSettingsView{}, err
	}
	gcMu.Lock()
	if gogc, _ := GCSettings(); target == math.MaxInt64 && gogc < 0 {
		gcMu.Unlock()
		t.mu.Unlock()
		return core.RuntimeSettingsView{}, ErrLimitNeededByCollector
	}
	debug.SetMemoryLimit(target)
	gcMu.Unlock()
	t.mem = m
	t.memStartVar = ""
	t.memApplied = target
	t.memBase = base
	t.mu.Unlock()
	return t.RuntimeSettings(), nil
}

// memoryTarget returns the soft limit m sets, with math.MaxInt64 for none,
// and where the limit it is a share of came from.
func (t *Tuner) memoryTarget(m core.MemoryLimit) (int64, string, error) {
	switch m.Mode {
	case core.MemoryAutomatic:
		limit, base := t.memoryLimit()
		if limit == 0 {
			// No limit to take a share of, as at a start that found none.
			return math.MaxInt64, base, nil
		}
		return shareOf(limit, automaticRatio), base, nil
	case core.MemoryRatio:
		if m.Ratio < core.MinMemoryRatio || m.Ratio > core.MaxMemoryRatio {
			return 0, "", fmt.Errorf("the memory share must be between %.2f and %.2f", core.MinMemoryRatio, core.MaxMemoryRatio)
		}
		limit, base := t.memoryLimit()
		if limit == 0 {
			return 0, "", errors.New("no container or machine memory limit could be read, so a share of it cannot be set")
		}
		return shareOf(limit, m.Ratio), base, nil
	case core.MemoryBytes:
		if m.Bytes < core.MinMemoryBytes {
			return 0, "", fmt.Errorf("the memory limit must be at least %d bytes", core.MinMemoryBytes)
		}
		return m.Bytes, core.SourceSet, nil
	case core.MemoryNone:
		return math.MaxInt64, core.SourceSet, nil
	default:
		return 0, "", fmt.Errorf("the memory mode must be %s, %s, %s or %s",
			core.MemoryAutomatic, core.MemoryRatio, core.MemoryBytes, core.MemoryNone)
	}
}

// SetGCPercent implements core.RuntimeTuning.
func (t *Tuner) SetGCPercent(percent int) (core.RuntimeSettingsView, error) {
	if os.Getenv("GOGC") != "" {
		return core.RuntimeSettingsView{}, &core.PinnedError{Variable: "GOGC"}
	}
	t.mu.Lock()
	if _, err := SetGCPercent(percent); err != nil {
		t.mu.Unlock()
		return core.RuntimeSettingsView{}, err
	}
	t.gcSet = &percent
	t.mu.Unlock()
	return t.RuntimeSettings(), nil
}

// SetDatabaseMaxConns implements core.RuntimeTuning. A lower cap closes
// connections as they come back to the pool, so nothing in flight fails.
func (t *Tuner) SetDatabaseMaxConns(n int) (core.RuntimeSettingsView, error) {
	if t.db == nil {
		return core.RuntimeSettingsView{}, core.ErrScalingPrimitiveMissing
	}
	if s := t.startSetting("DATABASE_MAX_CONNECTIONS"); s.Pinned {
		return core.RuntimeSettingsView{}, &core.PinnedError{Variable: s.Variable}
	}
	if n < 0 || n > core.MaxDatabaseConns {
		return core.RuntimeSettingsView{}, fmt.Errorf("database connections must be between 1 and %d, or 0 for the start value", core.MaxDatabaseConns)
	}
	size := n
	if size == 0 {
		size = int(t.cfg.DatabaseMaxConns)
	}
	t.mu.Lock()
	t.db.SetMaxOpenConns(size)
	// The idle pool matches the open pool, as at start, so a burst does not
	// close and reopen connections.
	t.db.SetMaxIdleConns(size)
	t.dbSet = n
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), crowdingCheckLimit)
	defer cancel()
	view := t.RuntimeSettings()
	view.Warning = db.PoolCrowdingWarning(ctx, t.db, t.dialect, int32(size))
	return view, nil
}

// RuntimeSettings implements core.RuntimeTuning.
func (t *Tuner) RuntimeSettings() core.RuntimeSettingsView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return core.RuntimeSettingsView{
		CPU:              t.cpuSetting(),
		Memory:           t.memorySetting(),
		GCPercent:        t.gcSetting(),
		DatabaseMaxConns: t.poolSetting(),
		ReplicaMaxConns:  t.replicaPoolSetting(),
		Backpressure:     t.backpressureSetting(),
		Pooler:           t.poolerSetting(),
	}
}

func (t *Tuner) cpuSetting() core.Setting {
	cur := runtime.GOMAXPROCS(0)
	s := core.Setting{Value: cur, Mode: core.CPUAutomatic}
	switch {
	case t.cpuStartVar != "":
		variable, pinned := t.cpuVariable()
		start := t.startSetting(variable)
		s.Mode, s.Source, s.Variable, s.Pinned = core.CPUFixed, start.Source, variable, pinned
		if variable == "GOMAXPROCS" {
			// The Go runtime reads it from the process environment itself.
			s.Source = core.SourceEnvironment
		}
	case t.cpuMode == core.CPUFixed:
		s.Mode, s.Source = core.CPUFixed, core.SourceSet
		if cur != t.cpuThreads {
			s.Source = core.SourceRunning
		}
	case t.cpuOne:
		s.Source, s.Live = core.SourceContainer, true
		if cur != 1 {
			s.Source = core.SourceRunning
		}
	default:
		s.Source, s.Live = core.SourceMachine, true
		// Go's default is the CPU limit rounded up, never below 2, and the
		// limit is the source only where that is below the machine's count.
		if limit := config.CgroupCPULimit(t.root); limit > 0 && max(2, int(math.Ceil(limit))) < t.hostCPUs {
			s.Source = core.SourceContainer
		}
	}
	return s
}

func (t *Tuner) memorySetting() core.Setting {
	cur := debug.SetMemoryLimit(-1)
	s := core.Setting{Value: memoryValue(cur), Mode: t.mem.Mode}
	if t.memStartVar != "" {
		variable, pinned := t.memoryVariable()
		s.Source, s.Variable, s.Pinned = core.SourceEnvironment, variable, pinned
		if variable == "GOMEMLIMIT" {
			s.Mode = core.MemoryBytes
		} else {
			s.Source = t.startSetting(variable).Source
			s.Ratio = automaticRatio
		}
		return s
	}
	switch t.mem.Mode {
	case core.MemoryAutomatic:
		s.Source, s.Ratio, s.Live = t.memBase, automaticRatio, true
	case core.MemoryRatio:
		s.Source, s.Ratio, s.Live = core.SourceSet, t.mem.Ratio, true
	default:
		s.Source = core.SourceSet
	}
	if cur != t.memApplied {
		s.Source = core.SourceRunning
	}
	return s
}

func (t *Tuner) gcSetting() core.Setting {
	gogc, _ := GCSettings()
	s := core.Setting{Value: int(gogc)}
	switch {
	case os.Getenv("GOGC") != "":
		s.Source, s.Variable, s.Pinned = core.SourceEnvironment, "GOGC", true
	case t.gcSet != nil && int(gogc) == *t.gcSet:
		s.Source = core.SourceSet
	case t.gcSet == nil && gogc == 100:
		s.Source = core.SourceDefault
	default:
		s.Source = core.SourceRunning
	}
	return s
}

func (t *Tuner) poolSetting() core.Setting {
	if t.db == nil {
		return core.Setting{Source: core.SourceDefault}
	}
	cur := t.db.Stats().MaxOpenConnections
	s := t.startSetting("DATABASE_MAX_CONNECTIONS")
	s.Value = cur
	want := int(t.cfg.DatabaseMaxConns)
	if t.dbSet > 0 {
		// A write replaces the start value, so the variable no longer
		// names where the value came from.
		s.Source, s.Variable, want = core.SourceSet, "", t.dbSet
	}
	if cur != want {
		s.Source = core.SourceRunning
	}
	return s
}

func (t *Tuner) replicaPoolSetting() core.Setting {
	s := t.startSetting("DATABASE_REPLICA_MAX_CONNS")
	s.StartOnly = true
	if t.cfg.DatabaseReplicaURL != "" {
		s.Value = int(t.cfg.DatabaseReplicaMaxConns)
	}
	return s
}

func (t *Tuner) backpressureSetting() core.Setting {
	if !t.cfg.BackpressureEnabled {
		s := t.startSetting("BACKPRESSURE_ENABLED")
		s.StartOnly = true
		return s
	}
	s := t.startSetting("BACKPRESSURE_MAX_INFLIGHT")
	s.StartOnly = true
	s.Value = t.cfg.BackpressureMaxInflight
	return s
}

func (t *Tuner) poolerSetting() core.Setting {
	s := t.startSetting("CONNECTION_POOLER")
	s.StartOnly = true
	s.Value = string(t.cfg.PoolerMode)
	return s
}

// startSetting reports where key's start value came from, and whether the
// deployment pins it: a value in the environment or the configuration file
// pins a key unless LYEVE_OVERRIDABLE or the file's own tag opened it.
func (t *Tuner) startSetting(key string) core.Setting {
	res := t.resolve(key)
	s := core.Setting{Source: core.SourceDefault}
	switch res.From {
	case config.SourceEnv:
		s.Source, s.Variable = core.SourceEnvironment, key
	case config.SourceFile:
		s.Source, s.Variable = core.SourceFile, key
	default:
		return s
	}
	s.Pinned = !res.Overridable
	return s
}

// cpuVariable names the variable that sets the thread count, and whether it
// pins it. GOMAXPROCS is read by the Go runtime itself, so it always pins.
func (t *Tuner) cpuVariable() (string, bool) {
	if os.Getenv("GOMAXPROCS") != "" {
		return "GOMAXPROCS", true
	}
	return t.positiveVariable("CPU_QUOTA")
}

// memoryVariable names the variable that sets the soft limit, and whether it
// pins it. GOMEMLIMIT is read by the Go runtime itself, so it always pins.
func (t *Tuner) memoryVariable() (string, bool) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return "GOMEMLIMIT", true
	}
	return t.positiveVariable("MEMORY_LIMIT_BYTES")
}

// positiveVariable reports key when the deployment sets it to a positive
// number, which is when the engine acts on it, and whether it pins it.
func (t *Tuner) positiveVariable(key string) (string, bool) {
	res := t.resolve(key)
	if res.From != config.SourceEnv && res.From != config.SourceFile {
		return "", false
	}
	if n, err := strconv.ParseInt(res.Value, 10, 64); err != nil || n <= 0 {
		return "", false
	}
	return key, !res.Overridable
}

func (t *Tuner) resolve(key string) config.Resolution {
	if t.resolver != nil {
		return t.resolver.Resolve(key)
	}
	return config.ActiveResolver().Resolve(key)
}

// memoryLimit returns the limit the soft limit is a share of while it
// follows the machine: the cgroup's, else the machine's memory, else 0. It
// names where the limit came from.
func (t *Tuner) memoryLimit() (int64, string) {
	if n := config.CgroupMemoryLimit(t.root); n > 0 {
		return n, core.SourceContainer
	}
	if n := config.MachineMemory(t.root); n > 0 {
		return n, core.SourceMachine
	}
	return 0, core.SourceDefault
}

// ratio is the share of the limit the soft limit follows.
func (t *Tuner) ratio() float64 {
	if t.mem.Mode == core.MemoryRatio {
		return t.mem.Ratio
	}
	return automaticRatio
}

func shareOf(limit int64, ratio float64) int64 {
	return int64(float64(limit) * ratio)
}

// memoryValue reports a soft limit, or nil for none.
func memoryValue(limit int64) any {
	if limit == math.MaxInt64 {
		return nil
	}
	return limit
}
