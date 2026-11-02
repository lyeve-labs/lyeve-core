package logging_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unsafe"

	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/internal/logstream"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tailer's ring and the sink's buffer keep an entry long after the call
// that logged it, so a stored value must not share the caller's memory or grow
// with the caller's input.
func TestEnrichHandler_CapsAndClonesStoredValues(t *testing.T) {
	tailer := observability.NewLogTailer()
	observability.SetGlobalTailer(tailer)
	t.Cleanup(func() { observability.SetGlobalTailer(nil) })

	h := logging.NewEnrichHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), nil)
	sink := &captureSink{}
	h.SetSink(sink)

	line := "GET /items?q=" + strings.Repeat("a", 4<<10)
	short := line[4:10]
	message := strings.Repeat("m", 2*logstream.MaxMessageBytes)
	slog.New(h).InfoContext(context.Background(), message,
		"path", short,
		"query", line[4:],
		"err", errors.New(strings.Repeat("e", 4<<10)),
		"status", 200,
	)

	stored := sink.snapshot()
	require.Len(t, stored, 1)
	recent := tailer.Recent(observability.TailFilter{}, 0)
	require.Len(t, recent, 1)
	for _, e := range []observability.LogEntry{stored[0], recent[0]} {
		assert.Len(t, e.Message, logstream.MaxMessageBytes)
		assert.Equal(t, "/items", e.Attrs["path"])
		assert.NotSame(t, unsafe.StringData(short), unsafe.StringData(e.Attrs["path"].(string)))
		assert.Len(t, e.Attrs["query"], logstream.MaxValueBytes)
		assert.Len(t, e.Attrs["err"], logstream.MaxValueBytes)
		assert.EqualValues(t, 200, e.Attrs["status"])
	}
}
