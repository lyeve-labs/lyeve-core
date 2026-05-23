package core

import "time"

// The log ring.
//
// The engine keeps a ring of the most recent log records: every record the
// slog handler chain emits is pushed into it before it reaches stdout, so it
// holds what the process said in the last few thousand lines, across every
// tenant and every plugin, without a database round trip. A plugin that
// serves the log stream reads it: a live stream over SSE, a snapshot for
// export, and the counters. The plugin cannot own the ring, because the chain
// that feeds it is built before any plugin starts, so the engine hands the
// plugin this role instead of the buffer.
//
// The plugin implements its own filtering: Snapshot takes a predicate rather
// than a query so the ring stays a plain buffer and the query vocabulary
// belongs to the route that serves it. A subscriber that does not drain its
// channel loses records rather than blocking the logger. The sequence on
// each record shows the gap, and Snapshot with a since filter fills it.
//
// Usage in the plugin:
//
//	if p, ok := host.(core.LogRingProvider); ok {
//	    ring = p.LogRing()
//	}
//	id, ch := ring.Subscribe(256)
//	defer ring.Unsubscribe(id)
//
// The ring holds every tenant's records: a plugin serving it decides what a
// caller may see.

// LogLevel is a log severity, numbered as slog numbers its levels so the
// wire form is the integer the admin already reads.
type LogLevel int

const (
	LogLevelDebug LogLevel = -4
	LogLevelInfo  LogLevel = 0
	LogLevelWarn  LogLevel = 4
	LogLevelError LogLevel = 8
)

// ParseLogLevel reads a level name in either case. An unknown name is Info,
// so a filter with a typo narrows nothing rather than everything.
func ParseLogLevel(s string) LogLevel {
	switch s {
	case "debug", "DEBUG":
		return LogLevelDebug
	case "info", "INFO":
		return LogLevelInfo
	case "warn", "warning", "WARN", "WARNING":
		return LogLevelWarn
	case "error", "ERROR":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// String is the upper-case level name.
func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// LogRecord is one record in the ring. The JSON tags are the wire form the
// stream and the export answer with. TenantID and Plugin are lifted out of
// the record's attributes when it carries a tenant_id or a plugin attribute,
// so a client can filter on them without reading every attribute. Sequence
// is assigned by the ring, increases by one per record and never repeats
// within a process.
type LogRecord struct {
	Timestamp time.Time         `json:"timestamp"`
	Level     LogLevel          `json:"level"`
	Message   string            `json:"message"`
	TenantID  string            `json:"tenant_id,omitempty"`
	Plugin    string            `json:"plugin,omitempty"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	Sequence  uint64            `json:"sequence"`
}

// LogRingStats is the ring's counters.
type LogRingStats struct {
	// Entries is how many records the ring holds now.
	Entries int `json:"entries"`
	// Capacity is how many it can hold before the oldest is overwritten.
	Capacity int `json:"capacity"`
	// Sequence is the sequence of the newest record, and the number of
	// records pushed since the process started.
	Sequence uint64 `json:"sequence"`
	// Subscribers is how many live subscriptions are open.
	Subscribers int `json:"subscribers"`
}

// LogRing is the engine's ring of recent log records.
type LogRing interface {
	// Subscribe opens a live feed of records pushed from now on, with a
	// channel buffer of size buffer. The subscriber owns the id and closes
	// the feed with Unsubscribe. A subscriber that falls behind by more than
	// the buffer misses records rather than stalling the logger.
	Subscribe(buffer int) (id uint64, ch <-chan LogRecord)
	// Unsubscribe closes the feed opened under id and its channel. Calling
	// it twice is harmless.
	Unsubscribe(id uint64)
	// Snapshot copies the records the ring holds, oldest first, keeping those
	// match accepts. A nil match keeps every record. Max caps the count and
	// 0 means no cap.
	Snapshot(match func(LogRecord) bool, max int) []LogRecord
	// Stats is the ring's counters at this moment.
	Stats() LogRingStats
}

// LogRingProvider is implemented by the engine host and forwarded by
// ScopedHost. A plugin type-asserts it. A host built outside the engine may
// not implement it, and the plugin then answers that the ring is not
// available.
type LogRingProvider interface {
	LogRing() LogRing
}
