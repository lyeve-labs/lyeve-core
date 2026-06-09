// Package logstream keeps the engine's ring of recent log records and the
// slog handler that feeds it. The records, the ring's stats and the LogRing
// role a plugin reads it through are declared in pkg/core. The engine serves
// no route for the ring: a plugin reads it through that role and serves it.
package logstream

import "github.com/lyeve-labs/lyeve-core/pkg/core"

// Level is a log severity level compatible with slog.
type Level = core.LogLevel

const (
	LevelDebug = core.LogLevelDebug
	LevelInfo  = core.LogLevelInfo
	LevelWarn  = core.LogLevelWarn
	LevelError = core.LogLevelError
)

// LevelFromString parses a log level string (case-insensitive).
// Returns LevelInfo for unrecognized values.
func LevelFromString(s string) Level { return core.ParseLogLevel(s) }

// Entry is a single structured log record stored in the ring buffer.
type Entry = core.LogRecord

// BufferStats reports the current state of the log buffer.
type BufferStats = core.LogRingStats
