package observability

import (
	"encoding/json"
	"log/slog"
)

// LogLevelJSON is a JSON-friendly slog.Level. It marshals as the level string
// (DEBUG, INFO, WARN, ERROR) but stores the int value internally.
type LogLevelJSON slog.Level

// MarshalJSON implements json.Marshaler.
func (l LogLevelJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(slog.Level(l).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (l *LogLevelJSON) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "DEBUG":
		*l = LogLevelJSON(slog.LevelDebug)
	case "INFO":
		*l = LogLevelJSON(slog.LevelInfo)
	case "WARN":
		*l = LogLevelJSON(slog.LevelWarn)
	case "ERROR":
		*l = LogLevelJSON(slog.LevelError)
	default:
		*l = LogLevelJSON(slog.LevelInfo)
	}
	return nil
}

// Level returns the slog.Level value.
func (l LogLevelJSON) Level() slog.Level { return slog.Level(l) }

// String returns the canonical level name.
func (l LogLevelJSON) String() string { return slog.Level(l).String() }

// LogLevelSnapshot is a point-in-time snapshot of log level configuration.
type LogLevelSnapshot struct {
	DefaultLevel slog.Level              `json:"default_level"`
	Tenants      map[string]LogLevelJSON `json:"tenants"`
	Plugins      map[string]LogLevelJSON `json:"plugins"`
}

// LogEntry is a single structured log entry ready for transmission to a sink.
type LogEntry struct {
	Timestamp string            `json:"timestamp"`
	Level     string            `json:"level"`
	Message   string            `json:"message"`
	Attrs     map[string]any    `json:"attrs"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// LogSink is a pluggable log sink backend. All methods must be safe for
// concurrent use.
type LogSink interface {
	Write(entry LogEntry)
	Close() error
	Name() string
}

// SetGlobalLogLeveler replaces the global enrich handler's LevelManager.
// Implemented by internal/logging.
var SetGlobalLogLeveler func(leveler LogLevelManager)

// SetGlobalLogSink attaches a LogSink to the global enrich handler.
// Implemented by internal/logging.
var SetGlobalLogSink func(sink LogSink)
