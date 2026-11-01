package plugin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/compliance"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
	"github.com/lyeve-labs/lyeve-core/pkg/plugintest"
)

// A person exporting their own data chooses sections, one per running plugin
// and one for the engine. The exporters carried no owner, so nothing could
// list what a running install would export, export part of it, or leave out
// a plugin that had stopped.

// startOnly starts exactly the plugins it names.
type startOnly map[string]bool

func (p startOnly) Plugin(name string) licensing.PluginGrant {
	return licensing.PluginGrant{Start: p[name]}
}

// notesPlugin registers its exporter from Start without naming itself, as
// the shipped plugins do.
type notesPlugin struct {
	name  string
	db    core.Host
	table string
}

func (p *notesPlugin) Name() string { return p.name }

func (p *notesPlugin) Start(context.Context, core.Host) error {
	compliance.RegisterSubjectExporter(&notesExporter{db: p.db, table: p.table, owner: "plugin", key: "plugin_notes"})
	return nil
}

func (p *notesPlugin) Stop(context.Context) error { return nil }

// notesExporter exports the subject's notes of one owner in the tenant ctx
// names.
type notesExporter struct {
	db                core.Host
	table, owner, key string
}

func (e *notesExporter) ExportSubject(ctx context.Context, identifier string) (map[string]any, error) {
	rows, err := e.db.Querier(ctx).Query(ctx,
		`SELECT note FROM `+e.table+` WHERE owner_name = $1 AND subject = $2 AND tenant_id = $3`,
		e.owner, identifier, core.TenantIDFromCtx(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"note": note})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	return map[string]any{e.key: out}, nil
}

func TestRunSubjectExportSections_ExportsTheChosenSectionsOfRunningPlugins(t *testing.T) {
	plugintest.ForEachDialect(t, func(t *testing.T, host core.Host) {
		ctx := context.Background()
		q := host.Querier(ctx)
		compliance.ResetSubjectErasers()
		t.Cleanup(compliance.ResetSubjectErasers)
		compliance.ResetSubjectExporters()
		t.Cleanup(compliance.ResetSubjectExporters)
		t.Cleanup(func() { compliance.SetExportSectionResolver(nil) })

		table := "notes_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
		_, err := q.Exec(ctx, `CREATE TABLE `+table+` (owner_name VARCHAR(20) NOT NULL, subject VARCHAR(64) NOT NULL, tenant_id VARCHAR(64) NOT NULL, note VARCHAR(64) NOT NULL)`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = q.Exec(context.Background(), `DROP TABLE `+table) })

		subject, other := uuid.NewString(), uuid.NewString()
		for _, row := range [][4]string{
			{"plugin", subject, "acme", "plugin row"},
			{"engine", subject, "acme", "engine row"},
			{"plugin", subject, "globex", "another tenant"},
			{"plugin", other, "acme", "another account"},
		} {
			_, err := q.Exec(ctx, `INSERT INTO `+table+` (owner_name, subject, tenant_id, note) VALUES ($1, $2, $3, $4)`,
				row[0], row[1], row[2], row[3])
			require.NoError(t, err)
		}

		name := "notes-" + uuid.NewString()[:8]
		plugin.RegisterPlugin(name, func() core.Plugin { return &notesPlugin{name: name, db: host, table: table} })
		t.Cleanup(func() { plugin.UnregisterPlugin(name) })
		a := plugin.NewActivator(host, nil)
		a.Resolve(startOnly{name: true}, "")
		require.NoError(t, a.Start(ctx))
		a.RegisterSubjectExporters()
		compliance.RegisterSubjectExporterAs(compliance.EngineExportSection,
			&notesExporter{db: host, table: table, owner: "engine", key: "engine_notes"})
		compliance.RegisterSubjectEraser(plugintest.SubjectAccounts(host))

		assert.Equal(t, []string{compliance.EngineExportSection, name}, compliance.SubjectExportSections(),
			"the running plugin and the engine, on %s", host.Dialect())

		acme := core.WithTenantID(ctx, "acme")
		res, err := compliance.RunSubjectExportSections(acme, subject, []string{name})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"plugin_notes": []map[string]any{{"note": "plugin row"}}}, res.Plugins,
			"only the chosen section, only the subject, only the request's tenant")
		assert.Equal(t, map[string][]string{name: {"plugin_notes"}}, res.Sections)
		assert.False(t, res.Incomplete)
		assert.Equal(t, []string{"acme"}, res.Scope.Tenants)

		res, err = compliance.RunSubjectExportSections(acme, subject, []string{compliance.EngineExportSection, name})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{
			"plugin_notes": []map[string]any{{"note": "plugin row"}},
			"engine_notes": []map[string]any{{"note": "engine row"}},
		}, res.Plugins)

		_, err = compliance.RunSubjectExportSections(acme, subject, []string{"billing"})
		require.ErrorIs(t, err, compliance.ErrExportSectionUnavailable, "a section nothing exports")
		_, err = compliance.RunSubjectExportSections(ctx, subject, []string{name})
		require.ErrorIs(t, err, core.ErrTenantRequired, "no tenant is never every tenant")

		// A plugin that stops offers no section and contributes nothing,
		// though its exporter stays registered.
		require.NoError(t, a.Stop(ctx))
		assert.Equal(t, []string{compliance.EngineExportSection}, compliance.SubjectExportSections())
		_, err = compliance.RunSubjectExportSections(acme, subject, []string{name})
		require.ErrorIs(t, err, compliance.ErrExportSectionUnavailable)
		res, err = compliance.RunSubjectExportSections(acme, subject, []string{compliance.EngineExportSection})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"engine_notes": []map[string]any{{"note": "engine row"}}}, res.Plugins)
	})
}
