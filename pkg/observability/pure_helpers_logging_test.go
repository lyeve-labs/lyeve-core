package observability

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logging.go

func TestLogLevelJSON_Marshal(t *testing.T) {
	for lvl, want := range map[slog.Level]string{
		slog.LevelDebug: `"DEBUG"`,
		slog.LevelInfo:  `"INFO"`,
		slog.LevelWarn:  `"WARN"`,
		slog.LevelError: `"ERROR"`,
	} {
		b, err := LogLevelJSON(lvl).MarshalJSON()
		require.NoError(t, err)
		assert.JSONEq(t, want, string(b))
	}
}

func TestLogLevelJSON_Unmarshal(t *testing.T) {
	cases := map[string]slog.Level{
		`"DEBUG"`:   slog.LevelDebug,
		`"INFO"`:    slog.LevelInfo,
		`"WARN"`:    slog.LevelWarn,
		`"ERROR"`:   slog.LevelError,
		`"unknown"`: slog.LevelInfo, // default fallback
	}
	for in, want := range cases {
		var l LogLevelJSON
		require.NoError(t, l.UnmarshalJSON([]byte(in)))
		assert.Equal(t, want, l.Level())
	}

	// Non-string JSON is a hard error.
	var bad LogLevelJSON
	assert.Error(t, bad.UnmarshalJSON([]byte(`123`)))
}

func TestLogLevelJSON_String(t *testing.T) {
	assert.Equal(t, "WARN", LogLevelJSON(slog.LevelWarn).String())
}

func TestLogLevelMap_Resolution(t *testing.T) {
	m := NewLogLevelMap(0) // 0 -> defaults to INFO
	assert.Equal(t, slog.LevelInfo, m.DefaultLevel())
	assert.Equal(t, slog.LevelInfo, m.Level("", ""))

	m.SetDefaultLevel(slog.LevelWarn)
	assert.Equal(t, slog.LevelWarn, m.Level("tenA", "plugX"))

	// plugin level beats default. Composite beats plugin.
	m.SetPluginLevel("plugX", slog.LevelError)
	assert.Equal(t, slog.LevelError, m.Level("", "plugX"))
	assert.Equal(t, slog.LevelError, m.Level("tenA", "plugX"))
	m.SetPluginLevel("tenA:plugX", slog.LevelDebug)
	assert.Equal(t, slog.LevelDebug, m.Level("tenA", "plugX"))

	// tenant level applies when no plugin override matches.
	m.SetTenantLevel("tenB", slog.LevelDebug)
	assert.Equal(t, slog.LevelDebug, m.Level("tenB", ""))
	assert.Equal(t, slog.LevelDebug, m.Level("tenB", "unmapped"))

	// setting a level to 0 deletes the override (falls back).
	m.SetPluginLevel("plugX", 0)
	assert.Equal(t, slog.LevelWarn, m.Level("", "plugX"))
	m.SetTenantLevel("tenB", 0)
	assert.Equal(t, slog.LevelWarn, m.Level("tenB", ""))
}

func TestLogLevelMap_SnapshotAndMerge(t *testing.T) {
	src := NewLogLevelMap(slog.LevelWarn)
	src.SetTenantLevel("tenA", slog.LevelError)
	src.SetPluginLevel("plugX", slog.LevelDebug)

	snap := src.Snapshot()
	assert.Equal(t, slog.LevelWarn, snap.DefaultLevel)
	assert.Equal(t, slog.LevelError, snap.Tenants["tenA"].Level())
	assert.Equal(t, slog.LevelDebug, snap.Plugins["plugX"].Level())

	dst := NewLogLevelMap(slog.LevelInfo)
	dst.Merge(snap)
	assert.Equal(t, slog.LevelWarn, dst.DefaultLevel())
	assert.Equal(t, slog.LevelError, dst.Level("tenA", ""))
	assert.Equal(t, slog.LevelDebug, dst.Level("", "plugX"))
}
