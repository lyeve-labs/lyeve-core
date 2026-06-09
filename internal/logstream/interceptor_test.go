package logstream_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterceptor_Handle(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	intercepted := logstream.NewInterceptor(handler, b)

	logger := slog.New(intercepted)
	logger.Info("test message", "key1", "val1")

	entries := b.Snapshot(nil, 0)
	require.Len(t, entries, 1)
	assert.Equal(t, "test message", entries[0].Message)
	assert.Equal(t, logstream.LevelInfo, entries[0].Level)
	assert.Equal(t, "val1", entries[0].Attrs["key1"])
}

func TestInterceptor_Enabled(t *testing.T) {
	b := logstream.NewBuffer(100)

	// Default handler enables all levels
	handler := slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelDebug})
	intercepted := logstream.NewInterceptor(handler, b)

	assert.True(t, intercepted.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, intercepted.Enabled(context.Background(), slog.LevelError))
}

func TestInterceptor_WithAttrs(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	base := logstream.NewInterceptor(handler, b)

	// Create new handler with pre-attrs
	withAttrs := base.WithAttrs([]slog.Attr{
		slog.String("preset", "value"),
	})

	logger := slog.New(withAttrs)
	logger.Info("msg")

	entries := b.Snapshot(nil, 0)
	require.Len(t, entries, 1)
	assert.Equal(t, "value", entries[0].Attrs["preset"])
}

func TestInterceptor_WithAttrs_Empty(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	base := logstream.NewInterceptor(handler, b)

	// Empty attrs should return same handler
	same := base.WithAttrs(nil)
	assert.Equal(t, base, same)
}

func TestInterceptor_WithGroup(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	base := logstream.NewInterceptor(handler, b)

	// WithGroup should not panic and should return a new handler
	grouped := base.WithGroup("mygroup")
	assert.NotNil(t, grouped)
	assert.NotEqual(t, base, grouped)
}

func TestInterceptor_WithGroup_Empty(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	base := logstream.NewInterceptor(handler, b)

	same := base.WithGroup("")
	assert.Equal(t, base, same)
}

func TestInterceptor_ExtractTenantAndPlugin(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	intercepted := logstream.NewInterceptor(handler, b)

	logger := slog.New(intercepted)
	logger.Info("msg", "tenant_id", "acme", "plugin", "graphql")

	entries := b.Snapshot(nil, 0)
	require.Len(t, entries, 1)
	assert.Equal(t, "acme", entries[0].TenantID)
	assert.Equal(t, "graphql", entries[0].Plugin)
}

func TestInterceptor_MultipleRecords(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	intercepted := logstream.NewInterceptor(handler, b)

	logger := slog.New(intercepted)
	logger.Info("first")
	logger.Warn("second")
	logger.Error("third")

	entries := b.Snapshot(nil, 0)
	require.Len(t, entries, 3)
	assert.Equal(t, logstream.LevelInfo, entries[0].Level)
	assert.Equal(t, logstream.LevelWarn, entries[1].Level)
	assert.Equal(t, logstream.LevelError, entries[2].Level)
}

func TestNewInterceptedLogger(t *testing.T) {
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, nil)
	logger := logstream.NewInterceptedLogger(handler, b)
	require.NotNil(t, logger)

	logger.Info("via intercepted logger")
	entries := b.Snapshot(nil, 0)
	require.Len(t, entries, 1)
	assert.Equal(t, "via intercepted logger", entries[0].Message)
}

func TestSlogToStreamLevel(t *testing.T) {
	// Test level conversion via interceptor behavior
	b := logstream.NewBuffer(100)
	handler := slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelDebug})
	intercepted := logstream.NewInterceptor(handler, b)
	logger := slog.New(intercepted)

	logger.Log(context.Background(), slog.LevelDebug, "d")
	logger.Log(context.Background(), slog.LevelInfo, "i")
	logger.Log(context.Background(), slog.LevelWarn, "w")
	logger.Log(context.Background(), slog.LevelError, "e")

	entries := b.Snapshot(nil, 0)
	require.Len(t, entries, 4)
	assert.Equal(t, logstream.LevelDebug, entries[0].Level)
	assert.Equal(t, logstream.LevelInfo, entries[1].Level)
	assert.Equal(t, logstream.LevelWarn, entries[2].Level)
	assert.Equal(t, logstream.LevelError, entries[3].Level)
}
