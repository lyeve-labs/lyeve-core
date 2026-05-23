package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Each router filters plugin routes by its own prefix, so a pattern under
// neither is dropped by both without a word and answers 404 to every caller.
// That reads as a broken handler rather than a route that was never mounted,
// so the router logs a warning for it.
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	fn()
	return buf.String()
}

func routesOf(decls ...core.RouteDecl) []plugin.PluginRoutes {
	return []plugin.PluginRoutes{{Name: "probe", Routes: decls}}
}

func TestWarnUnmountableRoutes_NamesARouteNoRouterWillMount(t *testing.T) {
	out := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(core.RouteDecl{
			Method: http.MethodGet, Pattern: "/api/regions", Group: core.GroupPublic,
		}))
	})

	require.Contains(t, out, "no prefix any router mounts")
	assert.Contains(t, out, "/api/regions")
	assert.Contains(t, out, "probe", "the warning must name the plugin to fix")
}

func TestWarnUnmountableRoutes_NamesAnUnknownGroup(t *testing.T) {
	out := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(core.RouteDecl{
			Method: http.MethodGet, Pattern: "/api/v1/regions", Group: core.RouteGroup("api"),
		}))
	})

	require.Contains(t, out, "unknown group")
	assert.Contains(t, out, "treated as authenticated",
		"the warning must say what happens, not just that it is wrong")
}

func TestWarnUnmountableRoutes_SaysNothingAboutAGoodRoute(t *testing.T) {
	out := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(
			core.RouteDecl{Method: http.MethodGet, Pattern: "/api/v1/regions", Group: core.GroupPublic},
			core.RouteDecl{Method: http.MethodPost, Pattern: "/api/admin/regions", Group: core.GroupSuperAdmin},
			core.RouteDecl{Method: http.MethodPost, Pattern: "/api/v1/regions/query", Group: core.GroupAuth, Scope: "regions:read"},
		))
	})

	assert.Empty(t, out, "a mountable route must not produce noise")
}

func TestWarnUnmountableRoutes_NamesAScopeThatCannotBeRead(t *testing.T) {
	out := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(core.RouteDecl{
			Method: http.MethodGet, Pattern: "/api/v1/regions", Group: core.GroupAuth, Scope: "regions",
		}))
	})

	require.Contains(t, out, "not resource:action")
	assert.Contains(t, out, "only a key granted everything reaches it",
		"the warning must say what happens, not just that it is wrong")
}

func TestWarnUnmountableRoutes_NamesAScopeNoGroupReads(t *testing.T) {
	out := captureWarnings(t, func() {
		WarnUnmountableRoutes(routesOf(core.RouteDecl{
			Method: http.MethodGet, Pattern: "/api/v1/regions", Group: core.GroupPublic, Scope: "regions:read",
		}))
	})

	require.Contains(t, out, "outside the authenticated group")
	assert.Contains(t, out, "/api/v1/regions")
}
