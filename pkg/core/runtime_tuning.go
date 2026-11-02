package core

import (
	"context"
	"errors"
)

// RuntimeTuning is the process's runtime and scaling settings as one role:
// the scheduler's thread count, Go's soft memory limit, the garbage
// collector's target and the size of the primary database pool, with the
// facts about the machine, the database and the replicas that a choice of
// those values depends on. A plugin that lets an operator tune the engine
// reads and sets them through it, and the host keeps the process-wide state.
//
// Every write applies to the running process at once and answers the view
// RuntimeSettings would return afterwards. Nothing persists: a restart boots
// from the deployment's configuration again, so a caller that wants a value
// to last stores it and sets it again at start.
//
// A value the deployment sets in the environment or the configuration file
// is pinned, and a write to it answers a PinnedError naming the variable.
// ErrScalingPrimitiveMissing answers a write to something this install runs
// without, such as the database pool of an engine with no database. Any
// other error is a bound or a refusal, worded to be shown as it is, and
// carries no driver text.
type RuntimeTuning interface {
	// RuntimeFacts reads the facts afresh on each call, so a resized
	// container shows at once. Each database read is bounded by ctx. A fact
	// that cannot be read is unknown with a reason, never a zero.
	RuntimeFacts(ctx context.Context) RuntimeFacts
	// RuntimeSettings reports each setting in force, read back from the
	// runtime and the pool rather than from what was last asked for.
	RuntimeSettings() RuntimeSettingsView

	// SetCPULimit fixes the scheduler at threads, from 1 to the number of
	// CPUs the process may use. 0 returns it to automatic, where it follows
	// the CPU limit while the process runs.
	SetCPULimit(threads int) (RuntimeSettingsView, error)
	// SetMemoryLimit sets Go's soft memory limit by m's mode. Setting none
	// is refused while the garbage collector is off.
	SetMemoryLimit(m MemoryLimit) (RuntimeSettingsView, error)
	// SetGCPercent sets the garbage collector's target. A negative percent
	// turns the collector off and is refused without a memory limit in force.
	SetGCPercent(percent int) (RuntimeSettingsView, error)
	// SetDatabaseMaxConns sizes the primary pool's open and idle connections,
	// from 1 to MaxDatabaseConns. 0 returns it to the size it started with.
	// The view carries a warning when the pool claims a large share of what
	// the database accepts.
	SetDatabaseMaxConns(n int) (RuntimeSettingsView, error)
}

// RuntimeTuningProvider is implemented by the engine host and forwarded by
// ScopedHost. A plugin type-asserts it. A host built without the role
// answers nil, and the plugin then reports the settings as unavailable.
type RuntimeTuningProvider interface {
	RuntimeTuning() RuntimeTuning
}

// MaxDatabaseConns bounds SetDatabaseMaxConns.
const MaxDatabaseConns = 10000

// The sources a Setting or a Fact names.
const (
	// SourceEnvironment is a variable in the process environment.
	SourceEnvironment = "environment"
	// SourceFile is the deployment's configuration file.
	SourceFile = "file"
	// SourceSet is a value set through RuntimeTuning.
	SourceSet = "set"
	// SourceContainer is the limit of the process's cgroup.
	SourceContainer = "container"
	// SourceMachine is the machine itself: its CPUs or its memory.
	SourceMachine = "machine"
	// SourceDefault is the value the engine or the Go runtime picks when
	// nothing sets one.
	SourceDefault = "default"
	// SourceRunning is a value changed on the running process another way,
	// such as through the engine's own garbage collector endpoint.
	SourceRunning = "running"
	// SourceDatabase is a fact the database reports about itself.
	SourceDatabase = "database"
	// SourceCluster is a fact the replicas' transport reports.
	SourceCluster = "cluster"
)

// The modes of the CPU setting.
const (
	// CPUAutomatic follows the CPU limit while the process runs.
	CPUAutomatic = "automatic"
	// CPUFixed holds one thread count.
	CPUFixed = "fixed"
)

// The modes of the memory setting.
const (
	// MemoryAutomatic is 85% of the cgroup's memory limit, or of the
	// machine's memory without one, and follows either while the process
	// runs.
	MemoryAutomatic = "automatic"
	// MemoryRatio is a share of the same limit, from MinMemoryRatio to
	// MaxMemoryRatio, and follows it too.
	MemoryRatio = "ratio"
	// MemoryBytes is a fixed byte count, at least MinMemoryBytes.
	MemoryBytes = "bytes"
	// MemoryNone sets no soft limit.
	MemoryNone = "none"
)

// The bounds of SetMemoryLimit.
const (
	MinMemoryRatio = 0.50
	MaxMemoryRatio = 0.95
	MinMemoryBytes = 64 << 20
)

// MemoryLimit is a change to Go's soft memory limit. Ratio is read in
// MemoryRatio mode and Bytes in MemoryBytes mode.
type MemoryLimit struct {
	Mode  string
	Ratio float64
	Bytes int64
}

// Fact is one thing the host read about the machine, the database or the
// replicas. Value is set only when Known is true. Reason says why a fact is
// unknown, in words that can be shown as they are, and ReasonCode says the
// same as one of the FactReason constants, for a caller that words its own
// sentences.
type Fact struct {
	Value  any    `json:"value"`
	Known  bool   `json:"known"`
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
	// ReasonCode and Grant stay out of the JSON form, so a caller that
	// serves a Fact as it is keeps its shape and decides how to name them.
	ReasonCode FactReason `json:"-"`
	// Grant names the grant or permission the database account lacks,
	// with FactReasonMissingGrant.
	Grant string `json:"-"`
}

// FactReason is why a fact is unknown.
type FactReason string

// The reasons a fact can be unknown.
const (
	// FactReasonNoCgroup: the container sets no such limit, or the
	// process's cgroup files cannot be read.
	FactReasonNoCgroup FactReason = "no_cgroup"
	// FactReasonNoProc: the machine's own figure cannot be read from /proc
	// on this system.
	FactReasonNoProc FactReason = "no_proc"
	// FactReasonNoDatabase: the engine runs without a database.
	FactReasonNoDatabase FactReason = "no_database"
	// FactReasonMissingGrant: the database account lacks the grant the read
	// needs, which Fact.Grant names.
	FactReasonMissingGrant FactReason = "missing_grant"
	// FactReasonUnreadable: the database did not answer the read.
	FactReasonUnreadable FactReason = "unreadable"
	// FactReasonNotOverSQL: the database does not report the value to a
	// client.
	FactReasonNotOverSQL FactReason = "not_over_sql"
	// FactReasonNoFixedLimit: the database sets no connection limit and
	// grows on demand, which a caller sizing a pool treats differently from
	// a limit it failed to read.
	FactReasonNoFixedLimit FactReason = "no_fixed_limit"
	// FactReasonNoRoster: no transport that keeps a roster of the replicas
	// is running.
	FactReasonNoRoster FactReason = "no_roster"
	// FactReasonNotChosenYet: the engine has not chosen the backend the fact
	// describes yet.
	FactReasonNotChosenYet FactReason = "not_chosen_yet"
)

// RuntimeFacts is what a choice of settings depends on.
type RuntimeFacts struct {
	// CPULimit is the cgroup's CPU limit in cores, as a float64 fraction.
	CPULimit Fact `json:"cpu_limit"`
	// MemoryLimit is the cgroup's memory limit in bytes, as an int64.
	MemoryLimit Fact `json:"memory_limit"`
	// HostCores is the number of CPUs the process may run on, as an int.
	HostCores Fact `json:"host_cores"`
	// HostMemory is the machine's total memory in bytes, as an int64.
	HostMemory Fact `json:"host_memory"`

	// Dialect names the database: postgres, mysql or mssql, or empty with
	// no database.
	Dialect string `json:"dialect"`
	// DatabaseConnLimit is the number of connections the database accepts
	// from ordinary sessions, as an int.
	DatabaseConnLimit Fact `json:"database_connection_limit"`
	// DatabaseConnsOpen is the number of client sessions open on the
	// database now, every replica's and every other client's, as an int.
	DatabaseConnsOpen Fact `json:"database_connections_open"`
	// DatabaseCores is the number of CPUs the database reports, as an int.
	DatabaseCores Fact `json:"database_cores"`

	// Replicas is the number of replicas running, this one included, as an
	// int. Only a transport that keeps a roster can count them.
	Replicas Fact `json:"replicas"`
	// TransportAttached reports whether a transport carries changes between
	// the replicas.
	TransportAttached bool `json:"transport_attached"`
	// SharedCache reports, as a bool, whether the cache backend in use is
	// shared by every replica rather than held in this process.
	SharedCache Fact `json:"shared_cache"`
}

// Setting is one value in force and where it came from.
type Setting struct {
	// Value is the value in force, or nil where none is: no soft memory
	// limit, backpressure off, or no database pool.
	Value any `json:"value"`
	// Mode is the CPU or memory setting's mode, and empty for the others.
	Mode string `json:"mode,omitempty"`
	// Ratio is the share of the limit in MemoryRatio mode, and the 0.85 of
	// MemoryAutomatic.
	Ratio float64 `json:"ratio,omitempty"`
	// Source is one of the Source constants.
	Source string `json:"source"`
	// Variable names the environment variable or configuration key when the
	// deployment sets the value.
	Variable string `json:"variable,omitempty"`
	// Pinned is true when the deployment holds the value, so a write to it
	// answers a PinnedError.
	Pinned bool `json:"pinned"`
	// Live is true when the value follows the cgroup or the machine while
	// the process runs.
	Live bool `json:"live"`
	// StartOnly is true for a value the engine reads only when it starts,
	// which no write reaches.
	StartOnly bool `json:"start_only"`
}

// RuntimeSettingsView is every setting in force.
type RuntimeSettingsView struct {
	CPU              Setting `json:"cpu"`
	Memory           Setting `json:"memory"`
	GCPercent        Setting `json:"gc_percent"`
	DatabaseMaxConns Setting `json:"database_max_connections"`
	// ReplicaMaxConns, Backpressure and Pooler apply only at start.
	ReplicaMaxConns Setting `json:"replica_max_connections"`
	Backpressure    Setting `json:"backpressure"`
	Pooler          Setting `json:"pooler"`
	// Warning is set by SetDatabaseMaxConns when the new pool claims a large
	// share of the database's connection limit.
	Warning string `json:"warning,omitempty"`
}

// ErrSettingPinned is wrapped by every PinnedError.
var ErrSettingPinned = errors.New("this setting is pinned by the deployment")

// PinnedError answers a write to a value the deployment sets. Variable
// names the environment variable or configuration key that holds it.
type PinnedError struct {
	Variable string
}

func (e *PinnedError) Error() string {
	return ErrSettingPinned.Error() + ": " + e.Variable + " sets it"
}

func (e *PinnedError) Unwrap() error { return ErrSettingPinned }
