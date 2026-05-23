package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// describedPlugin is a fake plugin that describes itself.
type describedPlugin struct {
	*fakePlugin
	manifest core.PluginManifest
}

func (p describedPlugin) Manifest() core.PluginManifest { return p.manifest }

// statelessDescribedPlugin describes itself and runs with no database.
type statelessDescribedPlugin struct{ describedPlugin }

func (statelessDescribedPlugin) StatelessCapable() bool { return true }

// registerEach compiles in exactly the plugins given, for the test.
func registerEach(t *testing.T, plugins ...core.Plugin) []string {
	t.Helper()
	resetPluginRegistry()
	t.Cleanup(resetPluginRegistry)
	names := make([]string, 0, len(plugins))
	for _, p := range plugins {
		RegisterPlugin(p.Name(), func() core.Plugin { return p })
		names = append(names, p.Name())
	}
	return names
}

func TestActivatorStatus_CarriesWhatAPluginSaysAboutItself(t *testing.T) {
	names := registerEach(t,
		describedPlugin{fakePlugin: newFakePlugin("search"), manifest: core.PluginManifest{
			Label: "  Search ", Description: "Ranked full-text search across every schema.",
			Category: core.CategoryContent, Maturity: core.MaturityStable,
		}},
		newFakePlugin("plain"),
	)
	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(ungated(names...), "")

	report := a.Status()
	search := statusOf(t, report, "search")
	require.NotNil(t, search.Manifest)
	assert.Equal(t, core.PluginManifest{
		Label: "Search", Description: "Ranked full-text search across every schema.",
		Category: core.CategoryContent, Maturity: core.MaturityStable,
	}, *search.Manifest, "the row carries the manifest normalized")
	assert.Nil(t, statusOf(t, report, "plain").Manifest, "a plugin that does not describe itself has no manifest")

	b, err := json.Marshal(report)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"manifest":{"label":"Search","description":"Ranked full-text search across every schema.","category":"content","maturity":"stable"}`)
	assert.Equal(t, 1, strings.Count(string(b), `"manifest"`), "a row with no manifest leaves the key out")
}

func TestActivatorStatus_AManifestWithNoLabelTakesThePluginName(t *testing.T) {
	names := registerEach(t, describedPlugin{fakePlugin: newFakePlugin("relay"), manifest: core.PluginManifest{
		Label: "   ", Category: core.CategoryAutomation,
	}})
	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(ungated(names...), "")

	m := statusOf(t, a.Status(), "relay").Manifest
	require.NotNil(t, m)
	assert.Equal(t, core.PluginManifest{Label: "relay", Category: core.CategoryAutomation}, *m)
}

func TestActivatorStatus_AnUnknownCategoryIsDroppedAndLogged(t *testing.T) {
	names := registerEach(t,
		describedPlugin{fakePlugin: newFakePlugin("odd"), manifest: core.PluginManifest{
			Label: "Odd", Category: "Widgets", Maturity: "alpha",
		}},
		describedPlugin{fakePlugin: newFakePlugin("fine"), manifest: core.PluginManifest{
			Label: "Fine", Category: core.CategoryInsight,
		}},
	)
	var logs bytes.Buffer
	a := NewActivator(testHost{}, slog.New(slog.NewTextHandler(&logs, nil)))
	a.Resolve(ungated(names...), "")

	m := statusOf(t, a.Status(), "odd").Manifest
	require.NotNil(t, m)
	assert.Equal(t, core.PluginManifest{Label: "Odd"}, *m, "a category and maturity outside the set are left out")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	var warned []string
	for _, l := range lines {
		if strings.Contains(l, "plugin manifest") {
			warned = append(warned, l)
		}
	}
	require.Len(t, warned, 1, "one line for the one plugin that named something unknown:\n%s", logs.String())
	assert.Contains(t, warned[0], "plugin=odd")
	assert.Contains(t, warned[0], "category=Widgets")
	assert.Contains(t, warned[0], "maturity=alpha")
}

func TestActivatorStatus_TheManifestIsACopy(t *testing.T) {
	names := registerEach(t, describedPlugin{fakePlugin: newFakePlugin("search"), manifest: core.PluginManifest{
		Label: "Search", Category: core.CategoryContent,
	}})
	a := NewActivator(testHost{}, silentLogger)
	a.Resolve(ungated(names...), "")

	first := statusOf(t, a.Status(), "search").Manifest
	require.NotNil(t, first)
	first.Label = "changed by a caller"
	assert.Equal(t, "Search", statusOf(t, a.Status(), "search").Manifest.Label)
}

// A stateless engine reports every compiled plugin, the ones it does not
// start included, so each row keeps what its plugin says about itself.
func TestActivatorStatus_StatelessRowsKeepTheManifest(t *testing.T) {
	names := registerEach(t,
		statelessDescribedPlugin{describedPlugin{fakePlugin: newFakePlugin("relay"), manifest: core.PluginManifest{
			Label: "Relay", Category: core.CategoryDelivery,
		}}},
		describedPlugin{fakePlugin: newFakePlugin("ledger"), manifest: core.PluginManifest{
			Label: "Ledger", Category: core.CategoryOperations, Maturity: core.MaturityBeta,
		}},
	)
	a := NewActivator(testHost{}, silentLogger)
	a.RequireStateless()
	a.Resolve(ungated(names...), "")
	require.NoError(t, a.Start(context.Background()))

	report := a.Status()
	relay := statusOf(t, report, "relay")
	assert.True(t, relay.Active)
	require.NotNil(t, relay.Manifest)
	assert.Equal(t, "Relay", relay.Manifest.Label)

	ledger := statusOf(t, report, "ledger")
	assert.False(t, ledger.Active)
	assert.Equal(t, needsDatabaseReason, ledger.Reason)
	require.NotNil(t, ledger.Manifest)
	assert.Equal(t, core.PluginManifest{Label: "Ledger", Category: core.CategoryOperations, Maturity: core.MaturityBeta}, *ledger.Manifest)
}
