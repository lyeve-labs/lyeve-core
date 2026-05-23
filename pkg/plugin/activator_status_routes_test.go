package plugin

import (
	"context"
	"net/http"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type routedPlugin struct{ name string }

func (p *routedPlugin) Name() string                           { return p.name }
func (p *routedPlugin) Start(context.Context, core.Host) error { return nil }
func (p *routedPlugin) Stop(context.Context) error             { return nil }

func (p *routedPlugin) Routes() []RouteDecl {
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	return []RouteDecl{
		{Method: http.MethodPost, Pattern: "/api/admin/routed/items", Handler: h, Group: core.GroupAdmin},
		{Method: http.MethodGet, Pattern: "/api/routed/items", Handler: h, Group: core.GroupPublic},
		{Method: http.MethodGet, Pattern: "/api/admin/routed/items", Handler: h, Group: core.GroupAdmin},
	}
}

func TestStatus_ListsTheRoutesARunningPluginServes(t *testing.T) {
	resetPluginRegistry()
	defer resetPluginRegistry()

	RegisterPlugin("routed", func() core.Plugin { return &routedPlugin{name: "routed"} })
	quiet := newFakePlugin("quiet")
	RegisterPlugin("quiet", func() core.Plugin { return quiet })
	idle := newFakePlugin("idle")
	RegisterPlugin("idle", func() core.Plugin { return idle })

	a := NewActivator(testHost{}, nil)
	a.logger = silentLogger
	a.Resolve(grants("routed", "quiet"), "")
	require.NoError(t, a.Start(context.Background()))

	report := a.Status()

	assert.Equal(t, []PluginRoute{
		{Method: http.MethodGet, Pattern: "/api/admin/routed/items", Group: core.GroupAdmin},
		{Method: http.MethodPost, Pattern: "/api/admin/routed/items", Group: core.GroupAdmin},
		{Method: http.MethodGet, Pattern: "/api/routed/items", Group: core.GroupPublic},
	}, statusOf(t, report, "routed").Routes)
	assert.Nil(t, statusOf(t, report, "quiet").Routes, "a plugin that declares no routes lists none")
	assert.Nil(t, statusOf(t, report, "idle").Routes, "a plugin that is not running serves nothing")
}
