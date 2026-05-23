package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	_ "github.com/lyeve-labs/lyeve-core/pkg/plugin" // links the registry bridge
)

type declaringPlugin struct {
	stubPlugin
	declared []string
}

func (p *declaringPlugin) DeclaredFeatures() []string { return p.declared }

var _ core.FeatureDeclarer = (*declaringPlugin)(nil)

func TestCompiledFeatureNames_ListsPluginsAndWhatTheyDeclare(t *testing.T) {
	core.ResetPluginRegistry()
	t.Cleanup(core.ResetPluginRegistry)

	assert.Equal(t, []string{}, core.CompiledFeatureNames(), "an empty registry names nothing, and never nil")

	core.RegisterPlugin("notes", func() core.Plugin { return &stubPlugin{name: "notes"} })
	core.RegisterPlugin("reports", func() core.Plugin {
		return &declaringPlugin{stubPlugin: stubPlugin{name: "reports"}, declared: []string{"reports-export", "notes", ""}}
	})

	assert.Equal(t, []string{"notes", "reports", "reports-export"}, core.CompiledFeatureNames(),
		"every plugin, every name one declares, sorted, once, and never an empty name")
}

func TestCapabilitySet_LimitReadsTheNamedCeiling(t *testing.T) {
	// The engine gives a ceiling's name no meaning of its own.
	const ceiling = "example.items"
	cases := []struct {
		name string
		caps core.CapabilitySet
		want int
	}{
		{name: "a ceiling the set carries", caps: core.CapabilitySet{Caps: map[string]int{ceiling: 3}}, want: 3},
		{name: "a lifted ceiling", caps: core.CapabilitySet{Caps: map[string]int{ceiling: 0}}, want: 0},
		{name: "a name the set does not carry is unlimited", caps: core.CapabilitySet{Caps: map[string]int{"other.thing": 2}}, want: 0},
		{name: "a set with no ceilings", caps: core.CapabilitySet{}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.caps.Limit(ceiling))
		})
	}
}
