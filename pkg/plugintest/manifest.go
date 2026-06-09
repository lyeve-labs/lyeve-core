package plugintest

import (
	"strings"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// RequireManifest fails unless p describes itself the way the plugin status
// will show it: p implements core.Describer, its manifest names a label and a
// category from the closed set, and nothing in it is cut or dropped on the way
// to the status. A plugin's own test calls it, so a manifest the engine would
// change fails in the plugin's suite rather than reading differently in the
// console.
func RequireManifest(t T, p core.Plugin) {
	t.Helper()
	d, ok := p.(core.Describer)
	if !ok {
		t.Errorf("plugin %s does not implement core.Describer, so the plugin status lists it by its name alone", p.Name())
		return
	}
	raw := d.Manifest()
	got := core.NormalizeManifest(raw)
	switch {
	case got.Label == "":
		t.Errorf("plugin %s: the manifest names no label", p.Name())
	case got.Label != raw.Label:
		t.Errorf("plugin %s: the label %q shows as %q: keep it to %d runes with no space around it", p.Name(), raw.Label, got.Label, core.ManifestLabelMax)
	}
	if got.Description != raw.Description {
		t.Errorf("plugin %s: the description shows as %q: keep it to %d runes with no space around it", p.Name(), got.Description, core.ManifestDescriptionMax)
	}
	if got.Category == "" {
		t.Errorf("plugin %s: the category %q is not one of %s", p.Name(), raw.Category, categoryList())
	}
	if got.Maturity != raw.Maturity {
		t.Errorf("plugin %s: the maturity %q is neither %q nor %q", p.Name(), raw.Maturity, core.MaturityStable, core.MaturityBeta)
	}
}

// categoryList names the closed set of categories for a failure message.
func categoryList() string {
	cats := core.PluginCategories()
	names := make([]string, len(cats))
	for i, c := range cats {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}
