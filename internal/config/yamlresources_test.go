package config

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The sections the tests below read. The kernel registers none of its own,
// so each is registered here the way the plugin that reads it registers it
// in a build that links that plugin.
func init() {
	core.RegisterResourceSection("flows")
	core.RegisterResourceSection("flow_variables")
	core.RegisterResourceSection("webhooks")
}

func TestLoadDeclaredSection_ReadsEntriesAndExpandsTheEnvironment(t *testing.T) {
	t.Setenv("ORDERS_SECRET", "s3cret")
	dir := writeTree(t, map[string]string{
		"lyeve.yaml":         "$include: hooks/*.yaml\nwebhooks:\n  - name: orders\n    url: https://orders.example.com\n    secret: ${ORDERS_SECRET}\n    max_retries: 3\n",
		"hooks/slack.yaml":   "webhooks:\n  - name: slack\n    url: https://hooks.example.com/${MISSING:-fallback}\n",
		"conf.d/50-bus.yaml": "webhooks:\n  - name: bus\n    url: https://bus.example.com\n",
	})

	docs, err := LoadDeclaredSection(filepath.Join(dir, "lyeve.yaml"), "webhooks")
	require.NoError(t, err)
	byName := map[string]map[string]any{}
	for _, d := range docs {
		var m map[string]any
		require.NoError(t, json.Unmarshal(d, &m))
		byName[m["name"].(string)] = m
	}
	require.Len(t, byName, 3)
	assert.Equal(t, "s3cret", byName["orders"]["secret"])
	assert.EqualValues(t, 3, byName["orders"]["max_retries"], "a number stays a number")
	assert.Equal(t, "https://hooks.example.com/fallback", byName["slack"]["url"])
}

func TestLoadDeclaredSection_AreNotConfigurationKeys(t *testing.T) {
	t.Setenv("ORDERS_SECRET", "s3cret")
	dir := writeTree(t, map[string]string{"lyeve.yaml": "flows:\n  - slug: a\nflow_variables:\n  - key: k\n    value: v\nwebhooks:\n  - name: orders\n    secret: ${ORDERS_SECRET}\n"})

	values, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Empty(t, values, "a declared secret must not become a setting the config route lists")
}

func TestLoadDeclaredSection_RefusesWhatIsNotAResourceList(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "webhooks:\n  orders: {url: x}\n"})

	_, err := LoadDeclaredSection(filepath.Join(dir, "lyeve.yaml"), "webhooks")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhooks must be a list")

	_, err = LoadDeclaredSection(filepath.Join(dir, "lyeve.yaml"), "database")
	require.Error(t, err, "only the resource sections are readable this way")
}

func TestLoadDeclaredSection_NoneIsNotAnError(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "rate_limit:\n  rps: 10\n"})

	docs, err := LoadDeclaredSection(filepath.Join(dir, "lyeve.yaml"), "flows")
	require.NoError(t, err)
	assert.Empty(t, docs)
}

// A section a plugin registers is read the way the sections above are: its
// entries come back through LoadDeclaredSection and none of them becomes a
// setting. The name is unique to this test, so registering it for the rest
// of the binary changes no other file's meaning.
func TestLoadDeclaredSection_ReadsARegisteredSection(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "probe_widgets:\n  - name: a\n    token: t0ken\n  - name: b\n"})
	path := filepath.Join(dir, "lyeve.yaml")

	_, err := LoadDeclaredSection(path, "probe_widgets")
	require.Error(t, err, "a section nobody registered is not readable")
	_, err = LoadFiles(path)
	require.Error(t, err, "an unregistered list of entries is read as a setting and refused")

	core.RegisterResourceSection("probe_widgets")

	docs, err := LoadDeclaredSection(path, "probe_widgets")
	require.NoError(t, err)
	require.Len(t, docs, 2)
	assert.JSONEq(t, `{"name":"a","token":"t0ken"}`, string(docs[0]))
	values, err := LoadFiles(path)
	require.NoError(t, err)
	assert.Empty(t, values, "a registered section must not become settings")
}
