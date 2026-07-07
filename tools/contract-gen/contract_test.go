package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syntheticPlugin is a parse-only Go source exercising every extraction path:
// const Name, Routes() with inferred and explicit literals, Publish with
// selector/string-literal/ident event types, RegisterCoveredTable with dedup.
const syntheticPlugin = `package plugin

import "context"

const Name = "example-plugin"

type Plugin struct{ host hostType }

func (p *Plugin) Routes() []plugin.RouteDecl {
	return []plugin.RouteDecl{
		{Method: "GET", Pattern: "/api/example", Group: plugin.GroupAdmin},
		{Method: "POST", Pattern: "/api/example/create", Group: plugin.GroupPublic},
		plugin.RouteDecl{Method: "DELETE", Pattern: "/api/example/{id}", Group: plugin.GroupSuperAdmin},
		{Method: "PATCH"}, // missing Pattern -> must be dropped
	}
}

func (p *Plugin) work(ctx context.Context, evt core.Event) {
	p.host.HookPublisher().Publish(ctx, core.Event{
		Type:   core.AfterCreate,
		Schema: "content",
		Data:   map[string]any{"id": nil, "title": nil},
	})
	p.host.HookPublisher().Publish(ctx, core.Event{
		Type:   "custom_event",
		Schema: "thing",
	})
	p.host.HookPublisher().Publish(ctx, evt)     // ident arg -> traced, no emit
	other.Frobnicate(ctx, core.Event{})       // non-Publish selector -> ignored
	core.RegisterCoveredTable("sys_example")
	core.RegisterCoveredTable("sys_example")  // duplicate -> deduped
	core.RegisterCoveredTable("sys_example_audit")
}
`

func writePlugin(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.go"), []byte(src), 0o644))
	return dir
}

func TestExtractContract(t *testing.T) {
	dir := writePlugin(t, syntheticPlugin)

	fx, err := extractContract(dir)
	require.NoError(t, err)
	require.NotNil(t, fx)

	assert.Equal(t, "example-plugin", fx.PluginName)

	assert.Equal(t, []string{"sys_example", "sys_example_audit"}, fx.TablesCovered)

	// The Method-only literal is dropped. The other three survive.
	require.Len(t, fx.RoutesExposed, 3)
	assert.Equal(t, "GET", fx.RoutesExposed[0].Method)
	assert.Equal(t, "/api/example", fx.RoutesExposed[0].Pattern)
	assert.Equal(t, "admin", string(fx.RoutesExposed[0].Group))
	assert.Equal(t, "POST", fx.RoutesExposed[1].Method)
	assert.Equal(t, "public", string(fx.RoutesExposed[1].Group))
	assert.Equal(t, "DELETE", fx.RoutesExposed[2].Method)
	assert.Equal(t, "/api/example/{id}", fx.RoutesExposed[2].Pattern)
	assert.Equal(t, "super_admin", string(fx.RoutesExposed[2].Group))

	// Selector-typed and string-literal events are captured, but ident-arg Publish is not.
	require.Len(t, fx.HooksEmitted, 2)
	assert.Equal(t, "after_create", string(fx.HooksEmitted[0].EventType))
	assert.Equal(t, "content", fx.HooksEmitted[0].Schema)
	assert.Equal(t, []string{"id", "title"}, fx.HooksEmitted[0].PayloadKeys)
	assert.Equal(t, "custom_event", string(fx.HooksEmitted[1].EventType))
	assert.Equal(t, "thing", fx.HooksEmitted[1].Schema)
	assert.Empty(t, fx.HooksEmitted[1].PayloadKeys)
}

// edgePlugin exercises defensive branches: unresolvable event types, too-few
// Publish args, non-literal RegisterCoveredTable args, and non-core route groups.
const edgePlugin = `package plugin

import "context"

func (p *Plugin) Routes() []plugin.RouteDecl {
	return []plugin.RouteDecl{
		{Method: "GET", Pattern: "/x", Group: other.GroupAdmin}, // non-core pkg -> group ""
		{Method: "GET", Pattern: "/y", Group: bareGroup},        // bare ident -> group ""
		other.RouteDecl{Method: "GET", Pattern: "/z"},           // inferred by fields, no group
	}
}

func (p *Plugin) work(ctx context.Context, kind eventKind) {
	p.host.HookPublisher().Publish(ctx, core.Event{Type: kind, Schema: "s"}) // ident type -> no emit
	p.host.HookPublisher().Publish(ctx)                                          // <2 args -> skip
	p.host.HookPublisher().Publish(ctx, core.Event{Data: notAMap})            // no type, non-map data
	core.RegisterCoveredTable(dynamicName)                                    // non-literal -> ignored
	core.RegisterCoveredTable()                                               // no args -> ignored
}
`

func TestExtractContract_DefensiveBranches(t *testing.T) {
	dir := writePlugin(t, edgePlugin)

	fx, err := extractContract(dir)
	require.NoError(t, err)

	// All three routes survive (Method+Pattern present) but none resolves a group.
	require.Len(t, fx.RoutesExposed, 3)
	for _, r := range fx.RoutesExposed {
		assert.Empty(t, string(r.Group), "route %s %s should have no resolved group", r.Method, r.Pattern)
	}
	assert.Empty(t, fx.HooksEmitted)
	assert.Empty(t, fx.TablesCovered)
}

func TestExtractContract_NameFallsBackToDir(t *testing.T) {
	// No `const Name` -> plugin name falls back to the directory basename.
	dir := writePlugin(t, "package plugin\n\nfunc noop() {}\n")

	fx, err := extractContract(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(dir), fx.PluginName)
	assert.Empty(t, fx.HooksEmitted)
	assert.Empty(t, fx.TablesCovered)
	assert.Empty(t, fx.RoutesExposed)
}

func TestExtractContract_ParseError(t *testing.T) {
	// A syntactically broken file makes the parser fail. The error is surfaced.
	dir := writePlugin(t, "package plugin\n\nfunc (") // truncated

	fx, err := extractContract(dir)
	require.Error(t, err)
	assert.Nil(t, fx)
	assert.Contains(t, err.Error(), "parse")
}

func TestToEventType(t *testing.T) {
	cases := map[string]string{
		"BeforeCreate": "before_create",
		"AfterCreate":  "after_create",
		"BeforeUpdate": "before_update",
		"AfterUpdate":  "after_update",
		"BeforeDelete": "before_delete",
		"AfterDelete":  "after_delete",
		"Unknownish":   "", // unrecognized constant -> empty
		"":             "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, toEventType(in))
		})
	}
}

func TestToRouteGroup(t *testing.T) {
	cases := map[string]string{
		"GroupPublic":     "public",
		"GroupAuth":       "auth",
		"GroupAdmin":      "admin",
		"GroupSuperAdmin": "super_admin",
		"GroupNope":       "", // unrecognized -> empty
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, toRouteGroup(in))
		})
	}
}
