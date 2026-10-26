package plugintest_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/observability"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A line about another tenant is filed under no tenant, and the level a
// tenant admin raised for their own tenant no longer reaches it, which is what
// a plugin test asserts through the capture.
func TestCaptureEngineLog_StampsWhatTheEngineStamps(t *testing.T) {
	levels := observability.NewLogLevelMap(slog.LevelInfo)
	levels.SetTenantLevel("acme", slog.LevelDebug)
	log := plugintest.CaptureEngineLog(t, levels)
	ctx := core.WithTenantID(context.Background(), "acme")

	slog.DebugContext(ctx, "own tenant at the raised level")
	slog.DebugContext(core.LogContextAbout(ctx, "globex"), "another tenant at the raised level")
	slog.InfoContext(core.LogContextAbout(ctx, "globex"), "another tenant at the install level")

	entries := log.Entries()
	require.Len(t, entries, 2)
	require.Equal(t, "own tenant at the raised level", entries[0].Message)
	require.Equal(t, "acme", entries[0].Attrs["tenant_id"])
	require.Equal(t, "another tenant at the install level", entries[1].Message)
	require.NotContains(t, entries[1].Attrs, "tenant_id")

	require.Empty(t, log.Mentioning("acme"), "the stamped tenant is not something a line says")
	slog.InfoContext(ctx, "export refused", "tenant", "acme")
	require.Len(t, log.Mentioning("acme"), 1)
	require.Equal(t, "export refused tenant=acme", plugintest.LineText(log.Mentioning("acme")[0]))
}
