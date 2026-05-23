package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	hashA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	hashB = "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"
)

func TestLoadDeclaredAPIKeys_ReadsTheSection(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": `
api_keys:
  - name: operator
    sha256: ` + "9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08" + `
    roles: [super_admin]
    expires_at: 2027-01-01T00:00:00Z
  - name: relay
    sha256: ` + hashB + `
    scopes: ["flows:write"]
`})

	keys, err := LoadDeclaredAPIKeys(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	require.Len(t, keys, 2)
	assert.Equal(t, "operator", keys[0].Name)
	assert.Equal(t, hashA, keys[0].SHA256, "the hash is lower-cased to the form the engine computes")
	assert.Equal(t, []string{"super_admin"}, keys[0].Roles)
	assert.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), keys[0].ExpiresAt.UTC())
	assert.Equal(t, []string{"flows:write"}, keys[1].Scopes)
}

func TestLoadDeclaredAPIKeys_AreNotConfigurationKeys(t *testing.T) {
	// A hash flattened into a setting would be listed by the config route.
	dir := writeTree(t, map[string]string{"lyeve.yaml": "api_keys:\n  - name: a\n    sha256: " + hashA + "\n    roles: [admin]\n"})

	values, err := LoadFiles(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Empty(t, values)
}

func TestLoadDeclaredAPIKeys_FollowsIncludesAndConfD(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"lyeve.yaml":          "$include: keys/*.yaml\n",
		"keys/operator.yaml":  "api_keys:\n  - name: operator\n    sha256: " + hashA + "\n    roles: [super_admin]\n",
		"conf.d/50-more.yaml": "api_keys:\n  - name: relay\n    sha256: " + hashB + "\n    scopes: [\"flows:write\"]\n",
	})

	keys, err := LoadDeclaredAPIKeys(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	var names []string
	for _, k := range keys {
		names = append(names, k.Name)
	}
	assert.ElementsMatch(t, []string{"operator", "relay"}, names)
}

func TestLoadDeclaredAPIKeys_NoneIsNotAnError(t *testing.T) {
	dir := writeTree(t, map[string]string{"lyeve.yaml": "rate_limit:\n  rps: 10\n"})

	keys, err := LoadDeclaredAPIKeys(filepath.Join(dir, "lyeve.yaml"))
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestLoadDeclaredAPIKeys_RefusesAMalformedEntry(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not a list", "api_keys:\n  operator: x\n", "must be a list"},
		{"no name", "api_keys:\n  - sha256: " + hashA + "\n    roles: [admin]\n", "name is required"},
		{"the key itself instead of its hash", "api_keys:\n  - name: a\n    sha256: operator-key-secret\n    roles: [admin]\n", "64 hex characters"},
		{"a short hash", "api_keys:\n  - name: a\n    sha256: abcd\n    roles: [admin]\n", "64 hex characters"},
		{"a role no gate reads", "api_keys:\n  - name: a\n    sha256: " + hashA + "\n    roles: [editor]\n", `role "editor"`},
		{"a scope with no action", "api_keys:\n  - name: a\n    sha256: " + hashA + "\n    scopes: [flows]\n", "resource:action"},
		{"a role narrowed by scopes", "api_keys:\n  - name: a\n    sha256: " + hashA + "\n    roles: [admin]\n    scopes: [\"flows:write\"]\n", "not both"},
		{"no roles and no scopes", "api_keys:\n  - name: a\n    sha256: " + hashA + "\n", "can reach nothing"},
		{"a name twice", "api_keys:\n  - name: a\n    sha256: " + hashA + "\n    roles: [admin]\n  - name: a\n    sha256: " + hashB + "\n    roles: [admin]\n", "already declared"},
		{"a hash twice", "api_keys:\n  - name: a\n    sha256: " + hashA + "\n    roles: [admin]\n  - name: b\n    sha256: " + hashA + "\n    roles: [admin]\n", "same sha256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"lyeve.yaml": tc.body})
			_, err := LoadDeclaredAPIKeys(filepath.Join(dir, "lyeve.yaml"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
