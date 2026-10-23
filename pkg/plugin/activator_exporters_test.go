package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type exporter struct{ calls int }

func (e *exporter) ExportSubject(_ context.Context, identifier string) (map[string]any, error) {
	e.calls++
	return map[string]any{"plugin_alpha_rows": []map[string]any{{"email": identifier}}}, nil
}

// exportingPlugin exposes its exporter rather than registering it during
// Start.
type exportingPlugin struct {
	*fakePlugin
	e *exporter
}

func (p *exportingPlugin) SubjectExporter() compliance.SubjectExporter { return p.e }

// The runtime calls this after activation. A plugin that exposes its exporter
// through compliance.SubjectExporterProvider, instead of registering it, is in
// the export exactly once, however many times the wiring runs.
func TestRegisterSubjectExporters_ReachesTheProviderInterface(t *testing.T) {
	t.Cleanup(resetPluginRegistry)
	compliance.ResetSubjectExporters()
	t.Cleanup(compliance.ResetSubjectExporters)

	provider := &exportingPlugin{fakePlugin: newFakePlugin("alpha"), e: &exporter{}}
	plain := newFakePlugin("beta")
	RegisterPlugin("alpha", func() core.Plugin { return provider })
	RegisterPlugin("beta", func() core.Plugin { return plain })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("alpha", "beta"), "")
	require.NoError(t, a.Start(context.Background()))

	assert.Equal(t, 1, a.RegisterSubjectExporters(), "the one plugin that exposes an exporter")
	assert.Equal(t, 1, a.RegisterSubjectExporters(), "a second pass reports the same plugin and registers nothing new")

	res, err := compliance.RunSubjectExport(context.Background(), "someone@example.com")
	require.NoError(t, err)
	assert.Equal(t, 1, provider.e.calls, "the exporter is asked once per export")
	assert.Contains(t, res.Plugins, "plugin_alpha_rows")
}

// An exporter the runtime takes from a plugin is that plugin's section. One
// registered from Start is attributed by the package that declares it, and
// one that two running plugins share a package with is in no section rather
// than in either.
func TestRegisterSubjectExporters_AttributesEachExporterToItsPlugin(t *testing.T) {
	t.Cleanup(resetPluginRegistry)
	compliance.ResetSubjectExporters()
	t.Cleanup(compliance.ResetSubjectExporters)
	t.Cleanup(func() { compliance.SetExportSectionResolver(nil) })

	provider := &exportingPlugin{fakePlugin: newFakePlugin("alpha"), e: &exporter{}}
	RegisterPlugin("alpha", func() core.Plugin { return provider })
	RegisterPlugin("beta", func() core.Plugin { return newFakePlugin("beta") })

	a := NewActivator(testHost{}, nil)
	a.Resolve(grants("alpha", "beta"), "")
	require.NoError(t, a.Start(context.Background()))
	compliance.RegisterSubjectExporter(&exporter{})
	a.RegisterSubjectExporters()

	assert.Equal(t, []string{"alpha"}, compliance.SubjectExportSections())
	section, active := a.subjectExportSection(&exporter{}, "")
	assert.Empty(t, section, "two running plugins share the exporter's package")
	assert.False(t, active)
}
