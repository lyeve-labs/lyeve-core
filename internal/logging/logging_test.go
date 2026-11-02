package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lyeve-labs/lyeve-core/internal/logging"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Context helpers

func TestWithLogger(t *testing.T) {
	l := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctx := logging.WithLogger(context.Background(), l)

	got := logging.Logger(ctx)
	assert.Equal(t, l, got)
}

func TestLogger_Fallback(t *testing.T) {
	got := logging.Logger(context.Background())
	assert.NotNil(t, got)
}

func TestWithPlugin(t *testing.T) {
	ctx := logging.WithPlugin(context.Background(), "graphql")
	assert.Equal(t, "graphql", logging.PluginFromCtx(ctx))
}

func TestPluginFromCtx_NotSet(t *testing.T) {
	assert.Equal(t, "", logging.PluginFromCtx(context.Background()))
}

func TestWithRequestID(t *testing.T) {
	ctx := logging.WithRequestID(context.Background(), "req-123")
	assert.Equal(t, "req-123", logging.RequestIDFromCtx(ctx))
}

func TestRequestIDFromCtx_NotSet(t *testing.T) {
	assert.Equal(t, "", logging.RequestIDFromCtx(context.Background()))
}

func TestWithLevel(t *testing.T) {
	ctx := logging.WithLevel(context.Background(), slog.LevelDebug)
	assert.Equal(t, slog.LevelDebug, logging.LevelFromCtx(ctx))
}

func TestLevelFromCtx_Default(t *testing.T) {
	assert.Equal(t, slog.LevelInfo, logging.LevelFromCtx(context.Background()))
}

// EnrichAttrs

func TestEnrichAttrs_Plugin(t *testing.T) {
	ctx := logging.WithPlugin(context.Background(), "myplugin")
	attrs := logging.EnrichAttrs(ctx)

	hasPlugin := false
	for _, a := range attrs {
		if a.Key == "plugin" && a.Value.String() == "myplugin" {
			hasPlugin = true
		}
	}
	assert.True(t, hasPlugin, "should contain plugin attr")
}

func TestEnrichAttrs_Empty(t *testing.T) {
	attrs := logging.EnrichAttrs(context.Background())
	assert.Len(t, attrs, 0)
}

// EnrichHandler

func TestNewDefaultHandler(t *testing.T) {
	h := logging.NewDefaultHandler()
	require.NotNil(t, h)

	assert.True(t, h.Enabled(context.Background(), slog.LevelInfo))
	assert.False(t, h.Enabled(context.Background(), slog.LevelDebug))
}

func TestEnrichHandler_Enabled_DebugBlocked(t *testing.T) {
	h := logging.NewDefaultHandler()
	assert.False(t, h.Enabled(context.Background(), slog.LevelDebug))
}

func TestEnrichHandler_Enabled_ErrorPasses(t *testing.T) {
	h := logging.NewDefaultHandler()
	assert.True(t, h.Enabled(context.Background(), slog.LevelError))
}

func TestEnrichHandler_Handle(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := logging.NewEnrichHandler(inner, nil)

	ctx := logging.WithPlugin(context.Background(), "testplugin")
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "hello world", 0)

	err := h.Handle(ctx, r)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "hello world")
	assert.Contains(t, buf.String(), "plugin")
	assert.Contains(t, buf.String(), "testplugin")
}

func TestEnrichHandler_Handle_WithTenant(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := logging.NewEnrichHandler(inner, nil)

	ctx := context.WithValue(context.Background(), core.TenantContextKey, "acme")
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "tenant log", 0)

	err := h.Handle(ctx, r)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "tenant_id")
	assert.Contains(t, buf.String(), "acme")
}

func TestEnrichHandler_Logger(t *testing.T) {
	h := logging.NewDefaultHandler()
	l := h.Logger()
	require.NotNil(t, l)
}

func TestEnrichHandler_WithAttrs(t *testing.T) {
	h := logging.NewDefaultHandler()
	h2 := h.WithAttrs([]slog.Attr{slog.String("static", "value")})

	assert.NotNil(t, h2)
	_, ok := h2.(*logging.EnrichHandler)
	assert.True(t, ok)
}

func TestEnrichHandler_WithGroup(t *testing.T) {
	h := logging.NewDefaultHandler()
	h2 := h.WithGroup("group1")
	assert.NotNil(t, h2)
}

func TestEnrichHandler_SetGetLeveler(t *testing.T) {
	h := logging.NewDefaultHandler()
	lvl := logging.NewLevelMap(slog.LevelWarn)
	h.SetLeveler(lvl)

	got := h.Leveler()
	assert.NotNil(t, got)
	assert.Equal(t, slog.LevelWarn, got.DefaultLevel())
}

func TestEnrichHandler_SetLeveler_Nil(t *testing.T) {
	h := logging.NewDefaultHandler()
	h.SetLeveler(nil)

	got := h.Leveler()
	assert.NotNil(t, got)
	assert.Equal(t, slog.LevelInfo, got.DefaultLevel())
}

func TestEnrichHandler_SetGetSink(t *testing.T) {
	h := logging.NewDefaultHandler()
	assert.Nil(t, h.Sink())

	h.SetSink(nil)
	assert.Nil(t, h.Sink())
}

func TestSetGlobalEnrichHandler(t *testing.T) {
	h := logging.NewDefaultHandler()
	logging.SetGlobalEnrichHandler(h)

	got := logging.GlobalEnrichHandler()
	assert.Equal(t, h, got)

	logging.SetGlobalEnrichHandler(nil)
}

func TestGlobalEnrichHandler_Nil(t *testing.T) {
	logging.SetGlobalEnrichHandler(nil)
	assert.Nil(t, logging.GlobalEnrichHandler())
}

// LevelMap

func TestNewLevelMap(t *testing.T) {
	lm := logging.NewLevelMap(slog.LevelWarn)
	require.NotNil(t, lm)
	assert.Equal(t, slog.LevelWarn, lm.DefaultLevel())
}

func TestLevelMap_Level_NoOverrides(t *testing.T) {
	lm := logging.NewLevelMap(slog.LevelInfo)
	assert.Equal(t, slog.LevelInfo, lm.Level("", ""))
	assert.Equal(t, slog.LevelInfo, lm.Level("any-tenant", "any-plugin"))
}

// A line logged while a request is served names who made it by id and the
// route serving it, never the caller's address.
func TestEnrichAttrs_NamesTheActorAndTheRoute(t *testing.T) {
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/api/admin/users/{id}"}
	base := context.WithValue(context.Background(), chi.RouteCtxKey, rctx)

	for _, tc := range []struct {
		name   string
		claims *core.AuthClaims
		want   string
	}{
		{"session", &core.AuthClaims{UserID: "u-1", Email: "ann@example.com"}, "u-1"},
		{"api key", &core.AuthClaims{UserID: "u-2", Email: "apikey:ci", IsAPIKey: true, APIKeyID: "k-1"}, "k-1"},
		{"admin token", &core.AuthClaims{UserID: "u-3", Email: "ann@example.com", AdminTokenID: "t-1"}, "u-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(base, core.ClaimsKey, tc.claims)
			got := map[string]string{}
			for _, a := range logging.EnrichAttrs(ctx) {
				got[a.Key] = a.Value.String()
				assert.NotContains(t, a.Value.String(), "@", "attribute %q carries an address", a.Key)
			}
			assert.Equal(t, tc.want, got["actor"])
			assert.Equal(t, "/api/admin/users/{id}", got["route"])
		})
	}

	assert.Empty(t, logging.EnrichAttrs(context.Background()), "an anonymous line outside a request carries nothing")
}
