package plugintest

import (
	"context"
	"reflect"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// declaringProbe is a registered plugin that gates on a name of its own
// beside the plugin name.
type declaringProbe struct{}

func (declaringProbe) Name() string                           { return "harness-probe" }
func (declaringProbe) Start(context.Context, core.Host) error { return nil }
func (declaringProbe) Stop(context.Context) error             { return nil }
func (declaringProbe) DeclaredFeatures() []string             { return []string{"harness-probe-export"} }

// plainProbe is a registered plugin that declares nothing.
type plainProbe struct{ name string }

func (p plainProbe) Name() string                         { return p.name }
func (plainProbe) Start(context.Context, core.Host) error { return nil }
func (plainProbe) Stop(context.Context) error             { return nil }

// The harness grants what a build that links no licensing implementation
// grants: each registered plugin's name and each name one declares through
// core.FeatureDeclarer, spelled as registered, and nothing else. A harness
// that granted a name no build grants would pass suites whose gates stay
// closed in production.
func TestDefaultCapabilities_GrantsExactlyWhatTheBuildCompiled(t *testing.T) {
	core.ResetPluginRegistry()
	t.Cleanup(core.ResetPluginRegistry)
	core.RegisterPlugin("harness-probe", func() core.Plugin { return declaringProbe{} })
	core.RegisterPlugin("multi-part-probe", func() core.Plugin { return plainProbe{name: "multi-part-probe"} })

	caps := defaultCapabilities()
	want := map[string]bool{"harness-probe": true, "harness-probe-export": true, "multi-part-probe": true}
	if !reflect.DeepEqual(caps.Features, want) {
		t.Errorf("harness granted %v, want %v", caps.Features, want)
	}
	if caps.Plan != "free" || caps.State != "free" {
		t.Errorf("harness plan and state = %q and %q, want free and free", caps.Plan, caps.State)
	}
}

// With nothing registered there is nothing to grant, and the set says so with
// an empty map rather than a nil one.
func TestDefaultCapabilities_AnEmptyRegistryGrantsNothing(t *testing.T) {
	core.ResetPluginRegistry()
	t.Cleanup(core.ResetPluginRegistry)

	caps := defaultCapabilities()
	if caps.Features == nil || len(caps.Features) != 0 {
		t.Errorf("an empty registry: harness granted %#v, want an empty map", caps.Features)
	}
}
