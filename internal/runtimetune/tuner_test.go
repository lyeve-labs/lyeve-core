package runtimetune_test

import (
	"context"
	"errors"
	"math"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/runtimetune"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// keepRuntime restores the thread count, the soft memory limit and the
// collector percentage when the test ends, since every one of them is
// process-wide. No test here runs in parallel for the same reason.
func keepRuntime(t *testing.T) {
	t.Helper()
	procs := runtime.GOMAXPROCS(0)
	limit := debug.SetMemoryLimit(-1)
	gogc, _ := runtimetune.GCSettings()
	t.Cleanup(func() {
		debug.SetMemoryLimit(limit)
		debug.SetGCPercent(int(gogc))
		runtime.SetDefaultGOMAXPROCS()
		if runtime.GOMAXPROCS(0) != procs {
			runtime.GOMAXPROCS(procs)
		}
	})
}

// unsetenv removes key for the test and restores it after.
func unsetenv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
}

// clean leaves no variable that would pin a setting, so the test decides.
func clean(t *testing.T) {
	t.Helper()
	keepRuntime(t)
	unsetenv(t, "GOMAXPROCS", "GOMEMLIMIT", "GOGC", "CPU_QUOTA", "MEMORY_LIMIT_BYTES",
		"DATABASE_MAX_CONNECTIONS", config.OverridableEnvKey)
}

// machine is a root holding a cgroup v2 container's CPU and memory limits
// and a machine with 8 GiB.
func machine(cpuMax, memMax string) fstest.MapFS {
	return fstest.MapFS{
		"proc/self/cgroup":         {Data: []byte("0::/\n")},
		"sys/fs/cgroup/cpu.max":    {Data: []byte(cpuMax + "\n")},
		"sys/fs/cgroup/memory.max": {Data: []byte(memMax + "\n")},
		"proc/meminfo":             {Data: []byte("MemTotal:        8388608 kB\n")},
	}
}

func setFile(root fstest.MapFS, name, data string) {
	root[name] = &fstest.MapFile{Data: []byte(data + "\n")}
}

// newTuner builds a tuner over root with a resolver that reads the test's
// environment, which a resolver reads when it is built.
func newTuner(root fstest.MapFS) *runtimetune.Tuner {
	return runtimetune.New(runtimetune.Options{Root: root, Resolver: config.NewResolver(nil)})
}

// goDefault is the thread count Go's documented rule gives on this machine:
// the CPUs the process may use, capped by the cgroup's limit rounded up but
// never below two, unless the machine has fewer.
func goDefault() int {
	n := runtime.NumCPU()
	if limit := config.CgroupCPULimit(os.DirFS("/")); limit > 0 && n >= 2 {
		n = min(n, max(2, int(math.Ceil(limit))))
	}
	return n
}

func requirePinned(t *testing.T, err error, variable string) {
	t.Helper()
	var pinned *core.PinnedError
	require.ErrorAs(t, err, &pinned)
	assert.Equal(t, variable, pinned.Variable)
	assert.ErrorIs(t, err, core.ErrSettingPinned)
}

func TestSetCPULimit_FixedReadsBackAndZeroRestoresGoDefault(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "max"))

	view, err := tuner.SetCPULimit(1)
	require.NoError(t, err)
	assert.Equal(t, 1, runtime.GOMAXPROCS(0))
	assert.Equal(t, core.Setting{Value: 1, Mode: core.CPUFixed, Source: core.SourceSet}, view.CPU)

	view, err = tuner.SetCPULimit(0)
	require.NoError(t, err)
	assert.Equal(t, goDefault(), runtime.GOMAXPROCS(0), "automatic must give back Go's default")
	assert.Equal(t, core.CPUAutomatic, view.CPU.Mode)
	assert.True(t, view.CPU.Live)
}

func TestSetCPULimit_ZeroUnderAOneCoreLimitHoldsOneThread(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("the engine's rule differs from Go's only on a machine with two or more CPUs")
	}
	clean(t)
	tuner := newTuner(machine("100000 100000", "max"))

	view, err := tuner.SetCPULimit(0)
	require.NoError(t, err)
	assert.Equal(t, 1, runtime.GOMAXPROCS(0))
	assert.Equal(t, core.Setting{Value: 1, Mode: core.CPUAutomatic, Source: core.SourceContainer, Live: true}, view.CPU)
}

func TestSetCPULimit_RefusesACountPastTheMachine(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "max"))
	before := runtime.GOMAXPROCS(0)

	_, err := tuner.SetCPULimit(runtime.NumCPU() + 1)
	require.Error(t, err)
	assert.NotErrorIs(t, err, core.ErrSettingPinned)
	_, err = tuner.SetCPULimit(-1)
	require.Error(t, err)
	assert.Equal(t, before, runtime.GOMAXPROCS(0))
}

func TestSetCPULimit_PinnedByTheEnvironment(t *testing.T) {
	for _, variable := range []string{"GOMAXPROCS", "CPU_QUOTA"} {
		t.Run(variable, func(t *testing.T) {
			clean(t)
			t.Setenv(variable, "2")
			tuner := newTuner(machine("max 100000", "max"))
			before := runtime.GOMAXPROCS(0)

			_, err := tuner.SetCPULimit(1)
			requirePinned(t, err, variable)
			assert.Equal(t, before, runtime.GOMAXPROCS(0))

			view := tuner.RuntimeSettings()
			assert.True(t, view.CPU.Pinned)
			assert.Equal(t, variable, view.CPU.Variable)
			assert.Equal(t, core.SourceEnvironment, view.CPU.Source)
		})
	}
}

// LYEVE_OVERRIDABLE opens CPU_QUOTA as it opens any key the engine resolves,
// while GOMAXPROCS, which the Go runtime reads itself, stays pinned.
func TestSetCPULimit_OpenedCPUQuotaAcceptsAWrite(t *testing.T) {
	clean(t)
	t.Setenv("CPU_QUOTA", "2")
	t.Setenv(config.OverridableEnvKey, "CPU_QUOTA")
	tuner := newTuner(machine("max 100000", "max"))

	view, err := tuner.SetCPULimit(1)
	require.NoError(t, err)
	assert.Equal(t, core.SourceSet, view.CPU.Source)
	assert.Equal(t, 1, runtime.GOMAXPROCS(0))
}

// The follower holds one thread while the CPU limit is one core or less and
// hands the count back to Go's default when the limit grows past it. A fixed
// count stops it.
func TestTick_FollowsTheCPULimit(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("the engine's rule differs from Go's only on a machine with two or more CPUs")
	}
	clean(t)
	root := machine("max 100000", "max")
	tuner := newTuner(root)

	setFile(root, "sys/fs/cgroup/cpu.max", "50000 100000")
	tuner.Tick()
	assert.Equal(t, 1, runtime.GOMAXPROCS(0), "a limit of half a core holds one thread")

	setFile(root, "sys/fs/cgroup/cpu.max", "max 100000")
	tuner.Tick()
	assert.Equal(t, goDefault(), runtime.GOMAXPROCS(0), "no limit gives back Go's default")

	_, err := tuner.SetCPULimit(2)
	require.NoError(t, err)
	setFile(root, "sys/fs/cgroup/cpu.max", "100000 100000")
	tuner.Tick()
	assert.Equal(t, 2, runtime.GOMAXPROCS(0), "a fixed count does not follow")
}

// The start applied one thread under a one-core limit, and the limit grew
// before the tuner was built. The tuner takes the start's count as given,
// rather than reading the limit again, so the next tick hands the count back
// to Go's default.
func TestTick_TakesTheStartsThreadCount(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("the engine's rule differs from Go's only on a machine with two or more CPUs")
	}
	clean(t)
	runtime.GOMAXPROCS(1)
	tuner := runtimetune.New(runtimetune.Options{
		Root:       machine("max 100000", "max"),
		Resolver:   config.NewResolver(nil),
		StartProcs: 1,
	})

	tuner.Tick()
	assert.Equal(t, goDefault(), runtime.GOMAXPROCS(0))
}

// A variable that LYEVE_OVERRIDABLE opened still set the start value, and
// nothing has replaced it, so the follower leaves both values as they
// started.
func TestTick_OpenedVariableHoldsItsStartValue(t *testing.T) {
	clean(t)
	t.Setenv("CPU_QUOTA", "2")
	t.Setenv("MEMORY_LIMIT_BYTES", "536870912")
	t.Setenv(config.OverridableEnvKey, "CPU_QUOTA,MEMORY_LIMIT_BYTES")
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(share(512*mib, 0.85))
	root := machine("max 100000", "1073741824")
	tuner := newTuner(root)

	setFile(root, "sys/fs/cgroup/cpu.max", "50000 100000")
	setFile(root, "sys/fs/cgroup/memory.max", "2147483648")
	tuner.Tick()
	assert.Equal(t, 2, runtime.GOMAXPROCS(0))
	assert.Equal(t, share(512*mib, 0.85), debug.SetMemoryLimit(-1))
	view := tuner.RuntimeSettings()
	assert.False(t, view.CPU.Pinned)
	assert.Equal(t, "CPU_QUOTA", view.CPU.Variable)
	assert.False(t, view.Memory.Pinned)
	assert.Equal(t, "MEMORY_LIMIT_BYTES", view.Memory.Variable)
}

// One core is the last limit that holds one thread, and anything above it
// gives the count back to Go's default.
func TestTick_OneCoreIsTheBoundary(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("the engine's rule differs from Go's only on a machine with two or more CPUs")
	}
	clean(t)
	root := machine("max 100000", "max")
	tuner := newTuner(root)

	for _, step := range []struct {
		cpuMax string
		want   int
	}{
		{cpuMax: "100000 100000", want: 1},
		{cpuMax: "100001 100000", want: goDefault()},
		{cpuMax: "100000 100000", want: 1},
		{cpuMax: "150000 100000", want: goDefault()},
	} {
		setFile(root, "sys/fs/cgroup/cpu.max", step.cpuMax)
		tuner.Tick()
		assert.Equal(t, step.want, runtime.GOMAXPROCS(0), "cpu.max %s", step.cpuMax)
	}
}

// Writes from plugins run against the follower's tick. The race detector
// reads this test, and the last write must still hold after a tick.
func TestTuner_SettersAgainstTheTick(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("50000 100000", "1073741824"))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				tuner.Tick()
			}
		}
	})
	for i := range 2000 {
		_, _ = tuner.SetCPULimit(i % 2)
		_, _ = tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryRatio, Ratio: 0.6})
		_, _ = tuner.SetGCPercent(100 + i%2)
		_ = tuner.RuntimeSettings()
		_ = tuner.RuntimeFacts(context.Background())
	}
	close(stop)
	wg.Wait()

	_, err := tuner.SetCPULimit(2)
	require.NoError(t, err)
	tuner.Tick()
	assert.Equal(t, 2, runtime.GOMAXPROCS(0), "a fixed count survives the tick")
	assert.Equal(t, share(gib, 0.6), debug.SetMemoryLimit(-1))
}

func TestSetMemoryLimit_EachModeReadsBack(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "1073741824"))

	tests := []struct {
		name   string
		limit  core.MemoryLimit
		want   int64
		source string
		live   bool
	}{
		{name: "ratio", limit: core.MemoryLimit{Mode: core.MemoryRatio, Ratio: 0.5}, want: 512 * mib, source: core.SourceSet, live: true},
		{name: "bytes", limit: core.MemoryLimit{Mode: core.MemoryBytes, Bytes: 100 * mib}, want: 100 * mib, source: core.SourceSet},
		{name: "none", limit: core.MemoryLimit{Mode: core.MemoryNone}, want: math.MaxInt64, source: core.SourceSet},
		{name: "automatic", limit: core.MemoryLimit{Mode: core.MemoryAutomatic}, want: share(gib, 0.85), source: core.SourceContainer, live: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view, err := tuner.SetMemoryLimit(tt.limit)
			require.NoError(t, err)
			assert.Equal(t, tt.want, debug.SetMemoryLimit(-1))
			assert.Equal(t, tt.limit.Mode, view.Memory.Mode)
			assert.Equal(t, tt.source, view.Memory.Source)
			assert.Equal(t, tt.live, view.Memory.Live)
			if tt.want == math.MaxInt64 {
				assert.Nil(t, view.Memory.Value)
			} else {
				assert.Equal(t, tt.want, view.Memory.Value)
			}
		})
	}
}

func TestSetMemoryLimit_RefusesOutOfBounds(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "1073741824"))
	before := debug.SetMemoryLimit(-1)

	for _, m := range []core.MemoryLimit{
		{Mode: core.MemoryRatio, Ratio: 0.49},
		{Mode: core.MemoryRatio, Ratio: 0.96},
		{Mode: core.MemoryBytes, Bytes: 64*mib - 1},
		{Mode: "half"},
	} {
		_, err := tuner.SetMemoryLimit(m)
		assert.Error(t, err, "%+v", m)
	}
	assert.Equal(t, before, debug.SetMemoryLimit(-1))
}

func TestSetMemoryLimit_PinnedByTheEnvironment(t *testing.T) {
	for _, variable := range []string{"GOMEMLIMIT", "MEMORY_LIMIT_BYTES"} {
		t.Run(variable, func(t *testing.T) {
			clean(t)
			// The variable is set after the start read it, so the soft limit
			// in force is the test's own and must survive the refusal.
			t.Setenv(variable, "536870912")
			tuner := newTuner(machine("max 100000", "1073741824"))
			before := debug.SetMemoryLimit(-1)

			_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryBytes, Bytes: 100 * mib})
			requirePinned(t, err, variable)
			assert.Equal(t, before, debug.SetMemoryLimit(-1))
			assert.True(t, tuner.RuntimeSettings().Memory.Pinned)
		})
	}
}

// Removing the soft limit while the collector is off would leave nothing
// bounding the heap, the GC endpoint's rule seen from the other side.
func TestSetMemoryLimit_NoneRefusedWhileTheCollectorIsOff(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "1073741824"))
	_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryAutomatic})
	require.NoError(t, err)
	_, err = tuner.SetGCPercent(-1)
	require.NoError(t, err)
	before := debug.SetMemoryLimit(-1)

	_, err = tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryNone})
	require.ErrorIs(t, err, runtimetune.ErrLimitNeededByCollector)
	assert.NotContains(t, err.Error(), "gogc", "the refusal is worded for the memory change")
	assert.Equal(t, before, debug.SetMemoryLimit(-1))
}

// While the soft limit is automatic or a ratio, the follower moves it when
// the container's limit changes. A byte count stops it.
func TestTick_FollowsTheMemoryLimit(t *testing.T) {
	clean(t)
	root := machine("max 100000", "1073741824")
	tuner := newTuner(root)
	_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryRatio, Ratio: 0.5})
	require.NoError(t, err)

	setFile(root, "sys/fs/cgroup/memory.max", "2147483648")
	tuner.Tick()
	assert.Equal(t, gib, debug.SetMemoryLimit(-1), "a ratio follows a larger container")

	delete(root, "sys/fs/cgroup/memory.max")
	_, err = tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryAutomatic})
	require.NoError(t, err)
	assert.Equal(t, share(8*gib, 0.85), debug.SetMemoryLimit(-1), "without a container limit the machine's memory applies")
	assert.Equal(t, core.SourceMachine, tuner.RuntimeSettings().Memory.Source)

	_, err = tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryBytes, Bytes: 256 * mib})
	require.NoError(t, err)
	setFile(root, "sys/fs/cgroup/memory.max", "536870912")
	tuner.Tick()
	assert.Equal(t, 256*mib, debug.SetMemoryLimit(-1), "a byte count does not follow")
}

// A limit pinned by MEMORY_LIMIT_BYTES does not follow the container.
func TestTick_PinnedMemoryDoesNotFollow(t *testing.T) {
	clean(t)
	t.Setenv("MEMORY_LIMIT_BYTES", "536870912")
	root := machine("max 100000", "1073741824")
	tuner := newTuner(root)
	before := debug.SetMemoryLimit(-1)

	setFile(root, "sys/fs/cgroup/memory.max", "2147483648")
	tuner.Tick()
	assert.Equal(t, before, debug.SetMemoryLimit(-1))
}

func TestSetGCPercent(t *testing.T) {
	t.Run("negative without a memory limit is refused and keeps the value", func(t *testing.T) {
		clean(t)
		debug.SetMemoryLimit(math.MaxInt64)
		tuner := newTuner(machine("max 100000", "max"))
		_, err := tuner.SetGCPercent(150)
		require.NoError(t, err)

		_, err = tuner.SetGCPercent(-1)
		require.ErrorIs(t, err, runtimetune.ErrCollectorOffNeedsLimit)
		gogc, _ := runtimetune.GCSettings()
		assert.Equal(t, int64(150), gogc)
	})
	t.Run("a value reads back as set, and a change made another way as running", func(t *testing.T) {
		clean(t)
		tuner := newTuner(machine("max 100000", "max"))
		view, err := tuner.SetGCPercent(200)
		require.NoError(t, err)
		assert.Equal(t, core.Setting{Value: 200, Source: core.SourceSet}, view.GCPercent)

		debug.SetGCPercent(300)
		assert.Equal(t, core.Setting{Value: 300, Source: core.SourceRunning}, tuner.RuntimeSettings().GCPercent)
	})
	t.Run("GOGC pins", func(t *testing.T) {
		clean(t)
		t.Setenv("GOGC", "100")
		tuner := newTuner(machine("max 100000", "max"))
		before, _ := runtimetune.GCSettings()

		_, err := tuner.SetGCPercent(200)
		requirePinned(t, err, "GOGC")
		after, _ := runtimetune.GCSettings()
		assert.Equal(t, before, after)
	})
}

// A byte limit above the machine's memory is capped at it, and a limit at
// or above that memory bounds nothing, so it does not let the collector go
// off or past the ceiling. Before the cap, a limit of 1<<62 let GOGC -1
// through with nothing bounding the heap.
func TestSetGCPercent_ALimitAtTheMachinesMemoryIsNoLimit(t *testing.T) {
	t.Run("a byte limit above the machine is capped, and the collector stays on", func(t *testing.T) {
		clean(t)
		tuner := newTuner(machine("max 100000", "max"))
		_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryBytes, Bytes: 1 << 62})
		require.NoError(t, err)
		assert.Equal(t, 8*gib, debug.SetMemoryLimit(-1), "capped at the machine's memory")

		_, err = tuner.SetGCPercent(-1)
		require.ErrorIs(t, err, runtimetune.ErrCollectorOffNeedsLimit)
		_, err = tuner.SetGCPercent(core.MaxGCPercent + 1)
		require.ErrorIs(t, err, runtimetune.ErrGCPercentNeedsLimit)
		gogc, _ := runtimetune.GCSettings()
		assert.Equal(t, int64(100), gogc)
	})
	t.Run("a percentage past the ceiling is refused without a limit", func(t *testing.T) {
		clean(t)
		tuner := newTuner(machine("max 100000", "max"))
		_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryNone})
		require.NoError(t, err)

		_, err = tuner.SetGCPercent(2_000_000_000)
		require.ErrorIs(t, err, runtimetune.ErrGCPercentNeedsLimit)
		_, err = tuner.SetGCPercent(core.MaxGCPercent)
		require.NoError(t, err)
	})
	t.Run("under a limit below the machine the ceiling does not apply", func(t *testing.T) {
		clean(t)
		tuner := newTuner(machine("max 100000", "1073741824"))
		_, err := tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryAutomatic})
		require.NoError(t, err)

		_, err = tuner.SetGCPercent(core.MaxGCPercent + 1)
		require.NoError(t, err)
		_, err = tuner.SetMemoryLimit(core.MemoryLimit{Mode: core.MemoryBytes, Bytes: 4 * gib})
		require.ErrorIs(t, err, runtimetune.ErrLimitNeededByGCPercent, "4 GiB is capped at the container's 1 GiB, which bounds nothing")
		assert.Equal(t, share(gib, 0.85), debug.SetMemoryLimit(-1))
	})
}

// fakeReplicas is a relay with a roster or without one.
type fakeReplicas struct {
	n        int
	known    bool
	attached bool
}

func (f fakeReplicas) Replicas(context.Context) (int, bool) { return f.n, f.known }
func (f fakeReplicas) Attached() bool                       { return f.attached }

// Every fact is known with its value or unknown with a reason, never a zero
// standing in for what could not be read.
func TestRuntimeFacts_MachineAndReplicas(t *testing.T) {
	clean(t)
	tuner := runtimetune.New(runtimetune.Options{
		Root:     machine("150000 100000", "1073741824"),
		Replicas: fakeReplicas{n: 3, known: true, attached: true},
		Resolver: config.NewResolver(nil),
	})
	f := tuner.RuntimeFacts(context.Background())
	requireReasonCodes(t, f)
	assert.Equal(t, core.Fact{Value: 1.5, Known: true, Source: core.SourceContainer}, f.CPULimit)
	assert.Equal(t, core.Fact{Value: gib, Known: true, Source: core.SourceContainer}, f.MemoryLimit)
	assert.Equal(t, core.Fact{Value: 8 * gib, Known: true, Source: core.SourceMachine}, f.HostMemory)
	assert.Equal(t, core.Fact{Value: runtime.NumCPU(), Known: true, Source: core.SourceMachine}, f.HostCores)
	assert.Equal(t, core.Fact{Value: 3, Known: true, Source: core.SourceCluster}, f.Replicas)
	assert.True(t, f.TransportAttached)

	for name, fact := range map[string]core.Fact{
		"database connection limit": f.DatabaseConnLimit,
		"database connections open": f.DatabaseConnsOpen,
		"database cores":            f.DatabaseCores,
		"shared cache":              f.SharedCache,
	} {
		assert.False(t, fact.Known, name)
		assert.Nil(t, fact.Value, name)
		assert.NotEmpty(t, fact.Reason, name)
	}

	tuner.SetSharedCache(true)
	assert.Equal(t, core.Fact{Value: true, Known: true}, tuner.RuntimeFacts(context.Background()).SharedCache)
}

func TestRuntimeFacts_NothingReadable(t *testing.T) {
	clean(t)
	tuner := runtimetune.New(runtimetune.Options{
		Root:     fstest.MapFS{},
		Replicas: fakeReplicas{n: 1},
		Resolver: config.NewResolver(nil),
	})
	f := tuner.RuntimeFacts(context.Background())
	requireReasonCodes(t, f)
	for name, fact := range map[string]core.Fact{
		"cpu limit":    f.CPULimit,
		"memory limit": f.MemoryLimit,
		"host memory":  f.HostMemory,
		"replicas":     f.Replicas,
	} {
		assert.False(t, fact.Known, name)
		assert.NotEmpty(t, fact.Reason, name)
	}
}

// An engine without a database still serves the role: the database facts
// are unknown and a pool write answers that the pool is missing.
func TestRuntimeTuning_WithoutADatabase(t *testing.T) {
	clean(t)
	tuner := newTuner(machine("max 100000", "max"))

	_, err := tuner.SetDatabaseMaxConns(5)
	assert.True(t, errors.Is(err, core.ErrScalingPrimitiveMissing), "err = %v", err)
	f := tuner.RuntimeFacts(context.Background())
	requireReasonCodes(t, f)
	assert.Equal(t, core.FactReasonNoDatabase, f.DatabaseConnLimit.ReasonCode)
	assert.Equal(t, "this engine runs without a database", f.DatabaseConnLimit.Reason)
	assert.Nil(t, tuner.RuntimeSettings().DatabaseMaxConns.Value)
}

// factReasons is every reason a fact can carry.
var factReasons = []core.FactReason{
	core.FactReasonNoCgroup, core.FactReasonNoProc, core.FactReasonNoDatabase, core.FactReasonMissingGrant,
	core.FactReasonUnreadable, core.FactReasonNotOverSQL, core.FactReasonNoFixedLimit, core.FactReasonNoRoster,
	core.FactReasonNotChosenYet,
}

// requireReasonCodes checks that every unknown fact in f carries one of the
// FactReason constants and its words, and a missing grant its name, so a
// caller maps each without reading the words. Every facts test calls it.
func requireReasonCodes(t *testing.T, f core.RuntimeFacts) {
	t.Helper()
	v := reflect.ValueOf(f)
	for i := range v.NumField() {
		fact, ok := v.Field(i).Interface().(core.Fact)
		if !ok || fact.Known {
			continue
		}
		name := v.Type().Field(i).Name
		assert.Contains(t, factReasons, fact.ReasonCode, "%s: %q", name, fact.Reason)
		assert.NotEmpty(t, fact.Reason, name)
		if fact.ReasonCode == core.FactReasonMissingGrant {
			assert.NotEmpty(t, fact.Grant, name)
		}
	}
}

// share is the soft limit a ratio of limit gives, rounded as the tuner does.
func share(limit int64, ratio float64) int64 { return int64(float64(limit) * ratio) }
