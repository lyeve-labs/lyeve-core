package provider

import "fmt"

// Capability is a bitmask of database engine feature flags. Callers branch on
// capability checks rather than string-comparing the engine name: this is
// compile-time-safe (new engines must declare their caps explicitly) and
// self-documenting (the flag name tells you what the conditional is about).
type Capability uint64

const (
	// DDL features
	CapCreateTableIfNotExists Capability = 1 << iota // CREATE TABLE IF NOT EXISTS
	// CapAddColumnIfNotExists indicates the engine supports ALTER TABLE ADD COLUMN IF NOT EXISTS.
	CapAddColumnIfNotExists // ALTER TABLE ADD COLUMN IF NOT EXISTS
	// CapDropColumnIfExists indicates the engine supports ALTER TABLE DROP COLUMN IF EXISTS.
	CapDropColumnIfExists // ALTER TABLE DROP COLUMN IF EXISTS

	// SQL features
	CapCTE // common table expressions (WITH)
	// CapCTERecursive indicates the engine supports recursive common table expressions.
	CapCTERecursive // recursive CTEs
	// CapWindowFunc indicates the engine supports window functions (OVER, PARTITION BY).
	CapWindowFunc // window functions (OVER, PARTITION BY)
	// CapJSONIndex indicates the engine supports JSON columns with index support.
	CapJSONIndex // JSON/JSONB with index support
	// CapFullText indicates the engine supports native full-text search.
	CapFullText // full-text search (native, not plugin)

	// Upsert support
	CapUpsertOnConflict // ON CONFLICT DO UPDATE (PostgreSQL)
	// CapUpsertOnDuplicate indicates the engine supports ON DUPLICATE KEY UPDATE (MySQL).
	CapUpsertOnDuplicate // ON DUPLICATE KEY UPDATE (MySQL)
	// CapUpsertMerge indicates the engine supports MERGE INTO (MSSQL).
	CapUpsertMerge // MERGE INTO (MSSQL)

	// Locking
	CapAdvisoryLock // session-level advisory locks
	// CapRowLock indicates the engine supports SELECT FOR UPDATE.
	CapRowLock // SELECT FOR UPDATE

	// Types
	CapUUIDNative // native UUID type (not CHAR(36))
	// CapJSONNative indicates the engine has a native JSON/JSONB type (not VARCHAR/MAX).
	CapJSONNative // native JSON/JSONB type (not VARCHAR/MAX)
	// CapTimestampTZ indicates the engine supports timestamp with timezone.
	CapTimestampTZ // timestamp with timezone
	// CapArrayType indicates the engine supports native array types (PostgreSQL).
	CapArrayType // native array type (PostgreSQL)

	// Transactions
	CapSavepoint // SAVEPOINT / ROLLBACK TO SAVEPOINT
	// CapTwoPhaseCommit indicates the engine supports two-phase commit (PREPARE TRANSACTION / XA).
	CapTwoPhaseCommit // PREPARE TRANSACTION / XA

	// Extensions
	CapListenNotify // LISTEN/NOTIFY (PostgreSQL)
	// CapPubSub indicates the engine supports generic pub/sub.
	CapPubSub // generic pub/sub. May need external broker
	// CapMaterializedView indicates the engine supports CREATE MATERIALIZED VIEW.
	CapMaterializedView // CREATE MATERIALIZED VIEW

	// Performance
	CapIndexInclude // covering indexes with INCLUDE clause
	// CapPartialIndex indicates the engine supports CREATE INDEX ... WHERE.
	CapPartialIndex // CREATE INDEX ... WHERE
	// CapParallelQuery indicates the engine supports parallel query execution.
	CapParallelQuery // parallel query execution
	// CapPartitionTable indicates the engine supports native table partitioning.
	CapPartitionTable // native table partitioning
)

// Capabilities is a concrete set of feature flags for a database engine.
type Capabilities struct {
	mask Capability
}

// NewCapabilities builds a Capabilities set from one or more flags.
func NewCapabilities(caps ...Capability) Capabilities {
	var m Capability
	for _, c := range caps {
		m |= c
	}
	return Capabilities{mask: m}
}

// Has reports whether all given capabilities are present.
func (c Capabilities) Has(need Capability) bool {
	return c.mask&need == need
}

// Mask returns the raw bitmask for debugging / serialization.
func (c Capabilities) Mask() Capability { return c.mask }

// String returns a compact description of the capability set.
func (c Capabilities) String() string {
	names := capabilityNames()
	result := ""
	for i, name := range names {
		if c.mask&(1<<i) != 0 {
			if result != "" {
				result += " "
			}
			result += name
		}
	}
	if result == "" {
		result = "(none)"
	}
	return result
}

// Name returns the human-readable name of a single capability flag.
func (c Capability) Name() string {
	if c == 0 {
		return "(zero)"
	}
	names := capabilityNames()
	for i, name := range names {
		if c == 1<<i {
			return name
		}
	}
	return fmt.Sprintf("Capability(%d)", c)
}

func capabilityNames() []string {
	return []string{
		"CreateTableIfNotExists", "AddColumnIfNotExists", "DropColumnIfExists",
		"CTE", "CTE_Recursive", "WindowFunc", "JSONIndex", "FullText",
		"Upsert:OnConflict", "Upsert:OnDuplicate", "Upsert:Merge",
		"AdvisoryLock", "RowLock",
		"UUID_Native", "JSON_Native", "TimestampTZ", "ArrayType",
		"Savepoint", "2PC",
		"ListenNotify", "PubSub", "MaterializedView",
		"IndexInclude", "PartialIndex", "ParallelQuery", "PartitionTable",
	}
}
