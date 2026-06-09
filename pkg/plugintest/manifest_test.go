package plugintest

import (
	"context"
	"strings"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// manifestPlugin is a plugin with no behavior, described as the test says.
type manifestPlugin struct{ manifest core.PluginManifest }

func (manifestPlugin) Name() string                           { return "probe" }
func (manifestPlugin) Start(context.Context, core.Host) error { return nil }
func (manifestPlugin) Stop(context.Context) error             { return nil }
func (p manifestPlugin) Manifest() core.PluginManifest        { return p.manifest }

// silentPlugin does not describe itself.
type silentPlugin struct{}

func (silentPlugin) Name() string                           { return "probe" }
func (silentPlugin) Start(context.Context, core.Host) error { return nil }
func (silentPlugin) Stop(context.Context) error             { return nil }

func TestRequireManifest_AcceptsAPluginThatDescribesItself(t *testing.T) {
	for _, m := range []core.PluginManifest{
		{Label: "Search", Description: "Ranked full-text search across every schema.", Category: core.CategoryContent, Maturity: core.MaturityStable},
		{Label: "Relay", Category: core.CategoryDelivery},
		{Label: strings.Repeat("a", core.ManifestLabelMax), Category: core.CategoryAI, Maturity: core.MaturityBeta},
	} {
		r := &recorder{}
		RequireManifest(r, manifestPlugin{manifest: m})
		if r.joined() != "" {
			t.Errorf("%+v was rejected:\n%s", m, r.joined())
		}
	}
}

func TestRequireManifest_EachRuleFires(t *testing.T) {
	cases := []struct {
		name  string
		p     core.Plugin
		wants string
	}{
		{"no describer", silentPlugin{}, "does not implement core.Describer"},
		{"no label", manifestPlugin{core.PluginManifest{Label: "  ", Category: core.CategoryContent}}, "names no label"},
		{"a label past the cap", manifestPlugin{core.PluginManifest{Label: strings.Repeat("a", core.ManifestLabelMax+1), Category: core.CategoryContent}}, "keep it to 40 runes"},
		{"space around the label", manifestPlugin{core.PluginManifest{Label: " Search", Category: core.CategoryContent}}, `the label " Search" shows as "Search"`},
		{"a description past the cap", manifestPlugin{core.PluginManifest{Label: "Search", Description: strings.Repeat("d", core.ManifestDescriptionMax+1), Category: core.CategoryContent}}, "keep it to 240 runes"},
		{"no category", manifestPlugin{core.PluginManifest{Label: "Search"}}, `the category "" is not one of content, delivery`},
		{"a category outside the set", manifestPlugin{core.PluginManifest{Label: "Search", Category: "Content"}}, `the category "Content" is not one of`},
		{"a maturity outside the set", manifestPlugin{core.PluginManifest{Label: "Search", Category: core.CategoryContent, Maturity: "alpha"}}, `the maturity "alpha" is neither "stable" nor "beta"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{}
			RequireManifest(r, c.p)
			if !strings.Contains(r.joined(), c.wants) {
				t.Errorf("expected a complaint containing %q, got:\n%s", c.wants, r.joined())
			}
		})
	}
}
