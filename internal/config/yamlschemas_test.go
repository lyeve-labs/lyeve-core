package config

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredNames reads the schema names out of a declarations document.
func declaredNames(t *testing.T, doc json.RawMessage) []string {
	t.Helper()
	if doc == nil {
		return nil
	}
	var parsed struct {
		Schemas []struct {
			Name string `json:"name"`
		} `json:"schemas"`
	}
	require.NoError(t, json.Unmarshal(doc, &parsed))
	var out []string
	for _, s := range parsed.Schemas {
		out = append(out, s.Name)
	}
	return out
}

func TestLoadDeclaredSchemas_ReadsTheSection(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
database:
  url: postgres://localhost/app
schemas:
  - name: article
    display_name: Article
    fields:
      - name: title
        field_type: text
        required: true
`})

	doc, err := LoadDeclaredSchemas(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Equal(t, []string{"article"}, declaredNames(t, doc))
}

func TestLoadDeclaredSchemas_AreNotConfigurationKeys(t *testing.T) {
	// Flattening them would turn every field into a configuration key named
	// after a column.
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
schemas:
  - name: article
    fields:
      - name: title
        field_type: text
`})

	values, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	for key := range values {
		assert.NotContains(t, key, "SCHEMAS", "key %q leaked from the schemas section", key)
	}
	assert.Empty(t, values)
}

func TestLoadDeclaredSchemas_FollowsIncludes(t *testing.T) {
	// Content types can live in their own files beside the settings.
	dir := writeTree(t, map[string]string{
		"lyeve.yaml":              "$include: schemas/*.yaml\ndatabase:\n  url: x\n",
		"schemas/10-article.yaml": "schemas:\n  - name: article\n    fields: [{name: title, field_type: text}]\n",
		"schemas/20-person.yaml":  "schemas:\n  - name: person\n    fields: [{name: name, field_type: text}]\n",
	})

	doc, err := LoadDeclaredSchemas(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"article", "person"}, declaredNames(t, doc))
}

func TestLoadDeclaredSchemas_MergesAcrossConfD(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"lyeve.yaml":          "schemas:\n  - name: article\n    fields: [{name: title, field_type: text}]\n",
		"conf.d/50-more.yaml": "schemas:\n  - name: person\n    fields: [{name: name, field_type: text}]\n",
	})

	doc, err := LoadDeclaredSchemas(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"article", "person"}, declaredNames(t, doc))
}

func TestLoadDeclaredSchemas_NoneIsNotAnError(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "database:\n  url: x\n"})

	doc, err := LoadDeclaredSchemas(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Nil(t, doc)
}

func TestLoadDeclaredSchemas_NoConfigFileIsNotAnError(t *testing.T) {
	t.Setenv(ConfigPathEnv, "")
	t.Chdir(t.TempDir())

	doc, err := LoadDeclaredSchemas("")
	require.NoError(t, err)
	assert.Nil(t, doc)
}

func TestLoadDeclaredSchemas_MissingPathStaysQuiet(t *testing.T) {
	// LoadFiles runs first and reports it. Reporting again would surface one
	// boot failure twice.
	doc, err := LoadDeclaredSchemas(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)
	assert.Nil(t, doc)
}

func TestLoadDeclaredSchemas_RejectsANonList(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "schemas:\n  article:\n    fields: []\n"})

	_, err := LoadDeclaredSchemas(filepath.Join(dir, "lyeve.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a list")
}
