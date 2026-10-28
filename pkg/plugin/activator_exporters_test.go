package plugin

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin/testdata/subexporter"
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

// sectionRun starts alpha, which exposes its exporter, registers extra as a
// plugin would from Start, and exports alpha's section. It returns what the
// activator logged and the bundle.
func sectionRun(t *testing.T, extra compliance.SubjectExporter) (string, *compliance.SubjectExportSectionsResult) {
	t.Helper()
	t.Cleanup(resetPluginRegistry)
	compliance.ResetSubjectExporters()
	t.Cleanup(compliance.ResetSubjectExporters)
	t.Cleanup(func() { compliance.SetExportSectionResolver(nil) })

	provider := &exportingPlugin{fakePlugin: newFakePlugin("alpha"), e: &exporter{}}
	RegisterPlugin("alpha", func() core.Plugin { return provider })

	var logs bytes.Buffer
	a := NewActivator(testHost{}, slog.New(slog.NewTextHandler(&logs, nil)))
	a.Resolve(grants("alpha"), "")
	require.NoError(t, a.Start(context.Background()))
	if extra != nil {
		compliance.RegisterSubjectExporter(extra)
	}
	a.RegisterSubjectExporters()

	res, err := compliance.RunSubjectExportSections(core.WithTenantID(context.Background(), "acme"),
		"someone@example.com", []string{"alpha"})
	require.NoError(t, err)
	return logs.String(), res
}

// An exporter kept in a subpackage of its plugin matches no plugin's
// package, so no section holds it and no self-service export can reach its
// data. The runtime has to say so at boot, and the bundle has to say it is
// short, or it reads as everything held about the person.
func TestRegisterSubjectExporters_ExporterInNoSectionIsLoggedAndLeavesTheExportIncomplete(t *testing.T) {
	logs, res := sectionRun(t, &subexporter.Exporter{})

	assert.Contains(t, logs, "level=ERROR")
	assert.Contains(t, logs, "index=0")
	assert.Contains(t, logs, "package=github.com/lyeve-labs/lyeve-core/pkg/plugin/testdata/subexporter")

	assert.Contains(t, res.Plugins, "plugin_alpha_rows", "the chosen section still exports")
	assert.NotContains(t, res.Plugins, "sub_rows")
	assert.True(t, res.Incomplete, "a bundle that cannot hold some data is not whole")
	require.Len(t, res.Errors, 1)
	assert.Equal(t, 0, res.Errors[0].Index)
	assert.Empty(t, res.Errors[0].Section)
}

// Every exporter in a section leaves nothing to log and nothing missing.
func TestRegisterSubjectExporters_ExportersInSectionsKeepTheExportComplete(t *testing.T) {
	logs, res := sectionRun(t, nil)

	assert.NotContains(t, logs, "level=ERROR")
	assert.Contains(t, res.Plugins, "plugin_alpha_rows")
	assert.False(t, res.Incomplete)
	assert.Empty(t, res.Errors)
}
