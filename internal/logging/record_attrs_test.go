package logging

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EnrichHandler copies a record's own attributes into the stored entry, not
// only the context-derived ones. Built from the context alone, everything a
// caller attached would go to the console and nowhere else:
//
//	slog.ErrorContext(ctx, "content create failed", "slug", req.Slug, "err", err)
//	stored: {"message":"content create failed","fields":{}}
//
// The stored log is the one an operator searches, and it would be the one
// without the slug and the error.

type captureSink struct{ entries []observability.LogEntry }

func (s *captureSink) Write(e observability.LogEntry) { s.entries = append(s.entries, e) }
func (s *captureSink) Close() error                   { return nil }
func (s *captureSink) Name() string                   { return "capture" }

func loggerWithSink(t *testing.T) (*slog.Logger, *captureSink) {
	t.Helper()
	sink := &captureSink{}
	h := NewEnrichHandler(slog.NewTextHandler(discard{}, nil), nil)
	h.SetSink(sink)
	return h.Logger(), sink
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestHandle_StoresTheAttributesTheCallerAttached(t *testing.T) {
	log, sink := loggerWithSink(t)

	log.ErrorContext(context.Background(), "content create failed",
		"slug", "my-entry", "count", 3)

	require.Len(t, sink.entries, 1)
	assert.Equal(t, "my-entry", sink.entries[0].Attrs["slug"])
	assert.EqualValues(t, 3, sink.entries[0].Attrs["count"])
}

// An error is the commonest attribute in this codebase, and encoding/json renders
// most error types as {} - so storing the value as-is would have swapped one
// missing detail for another.
func TestHandle_AnErrorAttributeIsStoredAsItsMessage(t *testing.T) {
	log, sink := loggerWithSink(t)

	log.ErrorContext(context.Background(), "upload failed", "err", errors.New("disk is full"))

	require.Len(t, sink.entries, 1)
	assert.Equal(t, "disk is full", sink.entries[0].Attrs["err"])
}

func TestHandle_AStringerAttributeIsStoredAsItsString(t *testing.T) {
	log, sink := loggerWithSink(t)
	u, err := url.Parse("https://example.com/hook")
	require.NoError(t, err)

	log.InfoContext(context.Background(), "dispatching", "target", u, "after", 5*time.Second)

	require.Len(t, sink.entries, 1)
	assert.Equal(t, "https://example.com/hook", sink.entries[0].Attrs["target"])
	assert.Equal(t, "5s", sink.entries[0].Attrs["after"])
}

// tenant_id and plugin are facts about the request, not something a log line
// gets to claim, so they are applied after the caller's attributes.
func TestHandle_ARecordCannotOverrideTheTenant(t *testing.T) {
	log, sink := loggerWithSink(t)
	ctx := context.WithValue(context.Background(), core.TenantContextKey, "acme")

	log.WarnContext(ctx, "suspicious", "tenant_id", "globex")

	require.Len(t, sink.entries, 1)
	assert.Equal(t, "acme", sink.entries[0].Attrs["tenant_id"],
		"a caller must not be able to file a line under another tenant")
}

func TestHandle_ARecordWinsOverAnOrdinaryContextAttribute(t *testing.T) {
	log, sink := loggerWithSink(t)
	ctx := WithRequestID(context.Background(), "ctx-request")

	log.InfoContext(ctx, "with an explicit value", "request_id", "explicit")

	require.Len(t, sink.entries, 1)
	assert.Equal(t, "explicit", sink.entries[0].Attrs["request_id"],
		"the caller is more specific than the context")
}

func TestHandle_NoAttributesIsStillAWellFormedEntry(t *testing.T) {
	log, sink := loggerWithSink(t)

	log.InfoContext(context.Background(), "plain")

	require.Len(t, sink.entries, 1)
	assert.Equal(t, "plain", sink.entries[0].Message)
	assert.NotNil(t, sink.entries[0].Attrs)
}
