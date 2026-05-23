package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTree materializes a set of relative paths as files under a temp
// directory and returns the directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	return dir
}

func TestLoadFiles_FlattensNestedKeys(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
database:
  url: postgres://localhost/app
  max_connections: 25
storage:
  driver: s3
  s3:
    bucket: media
    force_path_style: true
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "postgres://localhost/app", got["DATABASE_URL"].Value)
	assert.Equal(t, "25", got["DATABASE_MAX_CONNECTIONS"].Value)
	assert.Equal(t, "s3", got["STORAGE_DRIVER"].Value)
	assert.Equal(t, "media", got["STORAGE_S3_BUCKET"].Value)
	assert.Equal(t, "true", got["STORAGE_S3_FORCE_PATH_STYLE"].Value)
}

func TestLoadFiles_PluginsSectionDropsItsOwnSegment(t *testing.T) {
	// A plugin key already names its plugin, so plugins.example.host is the
	// setting a deployment knows as EXAMPLE_HOST, not PLUGINS_EXAMPLE_HOST.
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
plugins:
  example:
    host: mail.example.com
  idempotency:
    keyspace: orders
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "mail.example.com", got["EXAMPLE_HOST"].Value)
	assert.Equal(t, "orders", got["IDEMPOTENCY_KEYSPACE"].Value)
	assert.NotContains(t, got, "PLUGINS_EXAMPLE_HOST")
}

func TestLoadFiles_FlatKeysWorkAlongsideTheTree(t *testing.T) {
	// A .env can be transcribed a line at a time without learning the tree.
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
DATABASE_URL: postgres://localhost/app
storage:
  driver: local
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "postgres://localhost/app", got["DATABASE_URL"].Value)
	assert.Equal(t, "local", got["STORAGE_DRIVER"].Value)
}

func TestLoadFiles_ListsBecomeCommaSeparated(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
cors:
  origins:
    - https://a.example.com
    - https://b.example.com
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "https://a.example.com,https://b.example.com", got["CORS_ORIGINS"].Value)
}

func TestLoadFiles_RejectsCommaInsideAListEntry(t *testing.T) {
	// The engine splits list settings on commas, so an entry containing one
	// would silently become two.
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
cors:
  origins:
    - "https://a.example.com,https://b.example.com"
`})

	_, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains a comma")
}

func TestLoadFiles_OverridableTag(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
plugins:
  example:
    host: !overridable mail.example.com
    port: "587"
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.True(t, got["EXAMPLE_HOST"].Overridable, "the tagged key opens to the admin layer")
	assert.Equal(t, "mail.example.com", got["EXAMPLE_HOST"].Value)
	assert.False(t, got["EXAMPLE_PORT"].Overridable, "an untagged key stays locked")
}

func TestLoadFiles_OriginNamesFileAndLine(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "database:\n  url: postgres://localhost/app\n"})
	path := filepath.Join(dir, "lyeve.yaml")

	got, err := LoadFiles(path)
	require.NoError(t, err)

	assert.Equal(t, path+":2", got["DATABASE_URL"].Origin)
}

func TestLoadFiles_ExpandsVariables(t *testing.T) {
	t.Setenv("INJECTED_SECRET", "s3cret")
	t.Setenv("BLANK_VAR", "")
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
a: ${INJECTED_SECRET}
b: ${MISSING_VAR:-fallback}
c: ${MISSING_VAR}
d: prefix-${INJECTED_SECRET}-suffix
e: ${BLANK_VAR:-fallback}
f: no substitution $HERE
g: ${UNCLOSED
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "s3cret", got["A"].Value)
	assert.Equal(t, "fallback", got["B"].Value)
	assert.Equal(t, "", got["C"].Value)
	assert.Equal(t, "prefix-s3cret-suffix", got["D"].Value)
	assert.Equal(t, "fallback", got["E"].Value, "an empty variable takes the fallback")
	assert.Equal(t, "no substitution $HERE", got["F"].Value, "a bare dollar is left alone")
	assert.Equal(t, "${UNCLOSED", got["G"].Value, "an unclosed brace is left alone")
}

func TestLoadFiles_IncludeMergesBaseFirst(t *testing.T) {
	// The including file wins, which is what a base-plus-override layout reads
	// like.
	dir := writeTree(t, map[string]string{
		"lyeve.yaml": "$include: base.yaml\nstorage:\n  driver: s3\n",
		"base.yaml":  "storage:\n  driver: local\napp:\n  env: production\n",
	})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "s3", got["STORAGE_DRIVER"].Value, "the including file wins")
	assert.Equal(t, "production", got["APP_ENV"].Value, "the base supplies what the file omits")
}

func TestLoadFiles_IncludeAcceptsAListAndGlobs(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"lyeve.yaml":       "$include:\n  - parts/*.yaml\n",
		"parts/10-a.yaml":  "app:\n  env: first\nonly_in_a: yes\n",
		"parts/20-b.yaml":  "app:\n  env: second\n",
		"parts/ignore.txt": "not yaml",
	})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "second", got["APP_ENV"].Value, "a later glob match wins")
	assert.Equal(t, "yes", got["ONLY_IN_A"].Value)
}

func TestLoadFiles_IncludeCycleTerminates(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"lyeve.yaml": "$include: other.yaml\na: 1\n",
		"other.yaml": "$include: lyeve.yaml\nb: 2\n",
	})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err, "a cycle resolves rather than recursing to the depth limit")
	assert.Equal(t, "1", got["A"].Value)
	assert.Equal(t, "2", got["B"].Value)
}

func TestLoadFiles_IncludeMissingFileIsAnError(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "$include: nope.yaml\n"})

	_, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestLoadFiles_ConfDMergesAfterTheNamedFile(t *testing.T) {
	// A package can ship a base file that a deployment extends without editing
	// it, which is the convention operators expect from other services.
	dir := writeTree(t, map[string]string{
		"lyeve.yaml":          "app:\n  env: production\nkeep: me\n",
		"conf.d/50-over.yaml": "app:\n  env: staging\n",
	})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "staging", got["APP_ENV"].Value)
	assert.Equal(t, "me", got["KEEP"].Value)
}

func TestLoadFiles_DirectoryMergesInLexicalOrder(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"10-base.yaml":    "app:\n  env: production\n",
		"20-staging.yaml": "app:\n  env: staging\n",
		"notes.md":        "ignored",
	})

	got, err := LoadFiles(dir)
	require.NoError(t, err)

	assert.Equal(t, "staging", got["APP_ENV"].Value)
}

func TestLoadFiles_NoConfigIsNotAnError(t *testing.T) {
	// An engine configured entirely from the environment has no configuration
	// file.
	t.Setenv(ConfigPathEnv, "")
	t.Chdir(t.TempDir())

	got, err := LoadFiles("")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestLoadFiles_NamedButMissingPathIsAnError(t *testing.T) {
	// An operator who names a file and gets silence would run with settings
	// they believe are applied.
	_, err := LoadFiles(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestLoadFiles_ConfigPathEnvIsHonored(t *testing.T) {
	dir := writeTree(t, map[string]string{"custom.yaml": "app:\n  env: from-custom\n"})
	t.Setenv(ConfigPathEnv, filepath.Join(dir, "custom.yaml"))
	t.Chdir(t.TempDir())

	got, err := LoadFiles("")
	require.NoError(t, err)
	assert.Equal(t, "from-custom", got["APP_ENV"].Value)
}

func TestLoadFiles_DiscoversFileInWorkingDirectory(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "app:\n  env: discovered\n"})
	t.Setenv(ConfigPathEnv, "")
	t.Chdir(dir)

	got, err := LoadFiles("")
	require.NoError(t, err)
	assert.Equal(t, "discovered", got["APP_ENV"].Value)
}

func TestLoadFiles_EmptyFileContributesNothing(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "# only a comment\n"})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestLoadFiles_NullIsAnEmptyValue(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "example:\n  host:\n"})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	require.Contains(t, got, "EXAMPLE_HOST")
	assert.Equal(t, "", got["EXAMPLE_HOST"].Value)
}

func TestLoadFiles_RejectsNonMappingRoot(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "- a\n- b\n"})

	_, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a mapping")
}

func TestLoadFiles_ReportsParseErrorWithTheFileName(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "a: [unclosed\n"})

	_, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lyeve.yaml")
}

func TestLoadFiles_AnchorsExpand(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
defaults: &defaults
  driver: local
storage: *defaults
`})

	got, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "local", got["STORAGE_DRIVER"].Value)
}

func TestLoadFrom_FeedsTheResolver(t *testing.T) {
	// The end-to-end path: a file value reaches Config through the same field
	// an environment variable would have filled.
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
database:
  url: postgres://localhost/from-yaml
jwt:
  secret: a-secret-long-enough-for-validation
encryption:
  key: a-different-secret-long-enough-too
storage:
  driver: local
`})
	for _, k := range []string{"DATABASE_URL", "JWT_SECRET", "ENCRYPTION_KEY", "STORAGE_DRIVER"} {
		t.Setenv(k, "")
		require.NoError(t, os.Unsetenv(k))
	}
	t.Chdir(t.TempDir())

	cfg, err := LoadFrom(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "postgres://localhost/from-yaml", cfg.DatabaseURL)
	assert.Equal(t, "local", cfg.StorageDriver)
}

// The shipped example is the first thing an operator copies. A key that folds
// to a name the engine does not read would be invisible until something did not
// work, so the example is loaded here and checked against the real names.
func TestLoadFiles_ShippedExampleProducesTheDocumentedKeys(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/example")
	t.Setenv("JWT_SECRET", "example-secret")

	got, err := LoadFiles("../../lyeve.example.yaml")
	require.NoError(t, err)

	want := map[string]string{
		"APP_ENV":                  "production",
		"DATABASE_URL":             "postgres://localhost/example",
		"DATABASE_DRIVER":          "postgres",
		"DATABASE_MAX_CONNECTIONS": "25",
		"JWT_SECRET":               "example-secret",
		"JWT_EXPIRY_SECS":          "900",
		"ADMIN_LISTEN_ADDR":        "0.0.0.0:3001",
		"API_LISTEN_ADDR":          "0.0.0.0:3002",
		"SECURE_COOKIE":            "true",
		"CORS_ORIGINS":             "https://admin.example.com",
		"RATE_LIMIT_RPS":           "100",
		"RATE_LIMIT_BURST":         "300",
		"STORAGE_DRIVER":           "local",
		"STORAGE_LOCAL_PATH":       "./uploads",
		"MULTI_TENANT":             "false",
		"EXAMPLE_HOST":             "service.example.com",
		"EXAMPLE_PORT":             "8443",
		"GADGETS_ENABLED":          "true",
	}
	for key, value := range want {
		assert.Equal(t, value, got[key].Value, "key %q", key)
	}
	assert.True(t, got["EXAMPLE_HOST"].Overridable, "the example advertises example.host as overridable")
	assert.False(t, got["EXAMPLE_PORT"].Overridable)
}
