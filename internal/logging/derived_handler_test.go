package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plugins derive their loggers while the logging plugin's Start swaps the
// root's level manager and sink, so deriving and swapping must not race.
func TestEnrichHandler_DeriveWhileSettingIsRaceFree(t *testing.T) {
	root := logging.NewEnrichHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), nil)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				root.SetLeveler(logging.NewLevelMap(slog.LevelDebug))
				root.SetSink(&captureSink{})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h := root.WithAttrs([]slog.Attr{slog.Int("j", j)}).WithGroup("g")
				h.Enabled(context.Background(), slog.LevelInfo)
			}
		}()
	}
	wg.Wait()

	root.SetLeveler(logging.NewLevelMap(slog.LevelWarn))
	derived := root.WithAttrs([]slog.Attr{slog.String("k", "v")})
	assert.False(t, derived.Enabled(context.Background(), slog.LevelInfo), "the last level set does not hold")
}

// A logger derived before the logging plugin starts must still follow the
// level and reach the sink the plugin sets afterwards.
func TestEnrichHandler_DerivedFollowsLaterLevelerAndSink(t *testing.T) {
	ctx := context.Background()
	root := logging.NewEnrichHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), nil)

	derived := map[string]slog.Handler{
		"with attrs": root.WithAttrs([]slog.Attr{slog.String("k", "v")}),
		"with group": root.WithGroup("g"),
		"nested":     root.WithGroup("g").WithAttrs([]slog.Attr{slog.String("k", "v")}),
	}
	for name, h := range derived {
		require.False(t, h.Enabled(ctx, slog.LevelDebug), "%s: debug before the change", name)
	}

	root.SetLeveler(logging.NewLevelMap(slog.LevelDebug))
	sink := &captureSink{}
	root.SetSink(sink)

	for name, h := range derived {
		assert.True(t, h.Enabled(ctx, slog.LevelDebug), "%s: level change not followed", name)
		require.NoError(t, h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, name, 0)))
	}
	var got []string
	for _, e := range sink.snapshot() {
		got = append(got, e.Message)
	}
	assert.ElementsMatch(t, []string{"with attrs", "with group", "nested"}, got, "derived records missing from the sink")

	root.SetSink(nil)
	require.NoError(t, derived["nested"].Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "after detach", 0)))
	assert.Len(t, sink.snapshot(), 3, "a detached sink still receives derived records")
}
