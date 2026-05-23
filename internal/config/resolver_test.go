package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolver_Precedence(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		env         map[string]string
		file        map[string]FileValue
		admin       map[string]string
		wantValue   string
		wantSource  Source
		wantUnlock  bool
		description string
	}{
		{
			name:       "environment beats every other layer",
			key:        "EXAMPLE_HOST",
			env:        map[string]string{"EXAMPLE_HOST": "from-env"},
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file"}},
			admin:      map[string]string{"EXAMPLE_HOST": "from-admin"},
			wantValue:  "from-env",
			wantSource: SourceEnv,
		},
		{
			name:       "a locked file key beats the admin layer",
			key:        "EXAMPLE_HOST",
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file"}},
			admin:      map[string]string{"EXAMPLE_HOST": "from-admin"},
			wantValue:  "from-file",
			wantSource: SourceFile,
		},
		{
			name:       "an overridable file key yields to the admin layer",
			key:        "EXAMPLE_HOST",
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file", Overridable: true}},
			admin:      map[string]string{"EXAMPLE_HOST": "from-admin"},
			wantValue:  "from-admin",
			wantSource: SourceAdmin,
			wantUnlock: true,
		},
		{
			name:       "an overridable file key with nothing stored keeps its own value",
			key:        "EXAMPLE_HOST",
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file", Overridable: true}},
			wantValue:  "from-file",
			wantSource: SourceFile,
			wantUnlock: true,
		},
		{
			name:       "the environment still beats an overridable file key",
			key:        "EXAMPLE_HOST",
			env:        map[string]string{"EXAMPLE_HOST": "from-env"},
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file", Overridable: true}},
			admin:      map[string]string{"EXAMPLE_HOST": "from-admin"},
			wantValue:  "from-env",
			wantSource: SourceEnv,
		},
		{
			name:       "a key no file declares belongs to the admin layer",
			key:        "STRIPE_API_KEY",
			admin:      map[string]string{"STRIPE_API_KEY": "sk_live_x"},
			wantValue:  "sk_live_x",
			wantSource: SourceAdmin,
			wantUnlock: true,
		},
		{
			name:       "a key nobody sets resolves to nothing and stays editable",
			key:        "NOBODY_SETS_THIS",
			wantValue:  "",
			wantSource: SourceNone,
			wantUnlock: true,
		},
		{
			name:        "an explicitly empty variable pins the key to empty",
			key:         "EXAMPLE_HOST",
			env:         map[string]string{"EXAMPLE_HOST": ""},
			file:        map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file"}},
			admin:       map[string]string{"EXAMPLE_HOST": "from-admin"},
			wantValue:   "",
			wantSource:  SourceEnv,
			description: "setting a variable to empty is how an operator turns a setting off",
		},
		{
			name:       "an empty stored value does not shadow the file",
			key:        "EXAMPLE_HOST",
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file", Overridable: true}},
			admin:      map[string]string{"EXAMPLE_HOST": ""},
			wantValue:  "from-file",
			wantSource: SourceFile,
			wantUnlock: true,
		},
		{
			name:       "a plugin-style key reaches the same entry as the variable name",
			key:        "example_host",
			file:       map[string]FileValue{"EXAMPLE_HOST": {Value: "from-file"}},
			wantValue:  "from-file",
			wantSource: SourceFile,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			r := NewResolver(tc.file)
			if tc.admin != nil {
				r.SetAdminLayer(tc.admin)
			}

			got := r.Resolve(tc.key)
			assert.Equal(t, tc.wantValue, got.Value, tc.description)
			assert.Equal(t, tc.wantSource, got.From, "source")
			assert.Equal(t, tc.wantUnlock, got.Overridable, "overridable")
		})
	}
}

func TestResolver_OriginNamesTheFile(t *testing.T) {
	r := NewResolver(map[string]FileValue{
		"EXAMPLE_HOST": {Value: "mail.example.com", Origin: "lyeve.yaml:17"},
	})

	got := r.Resolve("EXAMPLE_HOST")
	assert.Equal(t, "lyeve.yaml:17", got.Origin)
}

func TestResolver_AdminLayerSwapsLive(t *testing.T) {
	r := NewResolver(nil)
	require.Equal(t, "", r.Get("STRIPE_API_KEY"))

	r.SetAdminLayer(map[string]string{"stripe_api_key": "first"})
	assert.Equal(t, "first", r.Get("STRIPE_API_KEY"))

	r.SetAdminLayer(map[string]string{"stripe_api_key": "second"})
	assert.Equal(t, "second", r.Get("STRIPE_API_KEY"))

	// A replacement that omits the key clears it rather than leaving the
	// previous value behind, which is what an operator deleting a setting
	// expects to happen.
	r.SetAdminLayer(map[string]string{})
	assert.Equal(t, "", r.Get("STRIPE_API_KEY"))
}

func TestResolver_Provenance(t *testing.T) {
	t.Setenv("PINNED_BY_ENV", "env-value")
	r := NewResolver(map[string]FileValue{
		"PINNED_BY_ENV":  {Value: "file-value", Origin: "lyeve.yaml:1"},
		"LOCKED_BY_FILE": {Value: "file-value", Origin: "lyeve.yaml:2"},
		"OPEN_TO_ADMIN":  {Value: "file-value", Origin: "lyeve.yaml:3", Overridable: true},
	})
	r.SetAdminLayer(map[string]string{"SET_IN_ADMIN": "admin-value"})

	byKey := map[string]Resolution{}
	for _, res := range r.Provenance() {
		byKey[res.Key] = res
	}

	require.Len(t, byKey, 4)
	assert.Equal(t, SourceEnv, byKey["PINNED_BY_ENV"].From)
	assert.False(t, byKey["PINNED_BY_ENV"].Overridable, "a variable pins the key")
	assert.Equal(t, SourceFile, byKey["LOCKED_BY_FILE"].From)
	assert.False(t, byKey["LOCKED_BY_FILE"].Overridable, "an untagged file key is locked")
	assert.Equal(t, SourceFile, byKey["OPEN_TO_ADMIN"].From)
	assert.True(t, byKey["OPEN_TO_ADMIN"].Overridable)
	assert.Equal(t, SourceAdmin, byKey["SET_IN_ADMIN"].From)
}

func TestResolver_ProvenanceIsSorted(t *testing.T) {
	r := NewResolver(map[string]FileValue{
		"ZULU": {Value: "z"}, "ALPHA": {Value: "a"}, "MIKE": {Value: "m"},
	})

	var keys []string
	for _, res := range r.Provenance() {
		keys = append(keys, res.Key)
	}
	assert.Equal(t, []string{"ALPHA", "MIKE", "ZULU"}, keys)
}

func TestResolver_BlankedKeys(t *testing.T) {
	t.Setenv("LEFTOVER_IN_DOTENV", "")
	t.Setenv("STILL_SET", "value")
	r := NewResolver(map[string]FileValue{
		"LEFTOVER_IN_DOTENV": {Value: "file-value"},
		"STILL_SET":          {Value: "file-value"},
		"NOT_IN_ENV":         {Value: "file-value"},
	})

	assert.Equal(t, []string{"LEFTOVER_IN_DOTENV"}, r.BlankedKeys(),
		"only a key an empty variable suppresses is reported")
}

func TestResolver_NilIsUsable(t *testing.T) {
	// The engine reads configuration before Load installs a resolver, and a
	// panic there would be a boot failure with no diagnostic.
	var r *Resolver
	assert.Equal(t, "", r.Get("ANYTHING"))
	assert.Nil(t, r.Provenance())
	assert.Nil(t, r.BlankedKeys())
	assert.Equal(t, 0, r.FileKeys())
	assert.NotPanics(t, func() { r.SetAdminLayer(map[string]string{"a": "b"}) })
}

func TestActiveResolver_NeverNil(t *testing.T) {
	prev := SetActiveResolver(nil)
	t.Cleanup(func() { SetActiveResolver(prev) })

	require.NotNil(t, ActiveResolver())
	assert.Equal(t, "", ActiveResolver().Get("ANY_KEY"))
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"example_host", "EXAMPLE_HOST"},
		{"EXAMPLE_HOST", "EXAMPLE_HOST"},
		{"storage.s3.bucket", "STORAGE_S3_BUCKET"},
		{"rate-limit-rps", "RATE_LIMIT_RPS"},
		{"  padded  ", "PADDED"},
		{"", ""},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, normalizeKey(tc.in), "normalizeKey(%q)", tc.in)
	}
}

func TestSource_String(t *testing.T) {
	assert.Equal(t, "env", SourceEnv.String())
	assert.Equal(t, "file", SourceFile.String())
	assert.Equal(t, "admin", SourceAdmin.String())
	assert.Equal(t, "default", SourceNone.String())
}

// A .env file becomes process environment rather than a file layer, so an
// install configured that way has every setting in the environment and none in
// any other layer. Provenance must still list them, or the admin cannot tell a
// pinned setting from one nobody has set.
func TestResolver_ProvenanceReportsASettingOnlyTheEnvironmentSupplies(t *testing.T) {
	t.Setenv("EXAMPLE_HOST", "relay.internal")
	r := NewResolver(nil)

	require.Empty(t, r.Provenance(), "nothing has been read yet")

	r.Get("EXAMPLE_HOST")

	byKey := map[string]Resolution{}
	for _, res := range r.Provenance() {
		byKey[res.Key] = res
	}
	require.Contains(t, byKey, "EXAMPLE_HOST")
	assert.Equal(t, SourceEnv, byKey["EXAMPLE_HOST"].From)
	assert.Equal(t, "relay.internal", byKey["EXAMPLE_HOST"].Value)
	assert.False(t, byKey["EXAMPLE_HOST"].Overridable,
		"the save handler refuses it, so the page must render it locked")
}

// The environment of a container is not configuration. Only a variable whose
// name the engine reads may appear, or an admin screen fills with unrelated
// process state.
func TestResolver_ProvenanceLeavesOutAVariableTheEngineNeverReads(t *testing.T) {
	t.Setenv("EXAMPLE_HOST", "relay.internal")
	t.Setenv("HOSTNAME", "container-7f3a")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not ours to show")
	r := NewResolver(nil)

	r.Get("EXAMPLE_HOST")

	var keys []string
	for _, res := range r.Provenance() {
		keys = append(keys, res.Key)
	}
	assert.Equal(t, []string{"EXAMPLE_HOST"}, keys)
}

// Resolve is called with names that arrive in a request body. Recording those
// would let a caller put any variable it could name on an admin screen.
func TestResolver_ResolveDoesNotWidenWhatProvenanceReports(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not ours to show")
	r := NewResolver(nil)

	r.Resolve("AWS_SECRET_ACCESS_KEY")

	assert.Empty(t, r.Provenance())
	assert.Empty(t, r.ReadKeys())
}

// The file and admin layers are reported whether or not anything has read them.
func TestResolver_ProvenanceStillReportsALayerNobodyHasRead(t *testing.T) {
	r := NewResolver(map[string]FileValue{"FROM_FILE": {Value: "v", Origin: "lyeve.yaml:1"}})
	r.SetAdminLayer(map[string]string{"FROM_ADMIN": "v"})

	var keys []string
	for _, res := range r.Provenance() {
		keys = append(keys, res.Key)
	}
	assert.Equal(t, []string{"FROM_ADMIN", "FROM_FILE"}, keys)
}

func TestResolver_ReadKeysRecordsEveryNameTheEngineAsksFor(t *testing.T) {
	r := NewResolver(nil)
	r.Get("DATABASE_URL")
	r.Get("example_host")
	r.Get("")

	assert.Equal(t, []string{"DATABASE_URL", "EXAMPLE_HOST"}, r.ReadKeys(),
		"names normalize, and an empty one is not a setting")
}

// An operator-only key decides where tenant data is encrypted or where
// backups go, so a value the admin layer holds for it must never take effect.
func TestResolver_OperatorOnlyKeyIgnoresTheAdminLayer(t *testing.T) {
	r := NewResolver(nil)
	r.SetAdminLayer(map[string]string{"kms_endpoint": "https://attacker.example", "example_host": "mail.example"})

	res := r.Resolve("KMS_ENDPOINT")
	assert.Equal(t, "", res.Value, "the stored value must not apply")
	assert.False(t, res.Overridable)
	assert.True(t, res.OperatorOnly)
	assert.Equal(t, []string{"KMS_ENDPOINT"}, r.IgnoredAdminKeys(), "boot names the ignored value")

	// An ordinary key still takes the admin layer.
	assert.Equal(t, "mail.example", r.Get("EXAMPLE_HOST"))
}

func TestResolver_OperatorOnlyKeyReadsTheFileAndTheEnvironment(t *testing.T) {
	r := NewResolver(map[string]FileValue{"KMS_PROVIDER": {Value: "aws", Overridable: true}})
	r.SetAdminLayer(map[string]string{"kms_provider": "local"})

	res := r.Resolve("kms_provider")
	assert.Equal(t, "aws", res.Value, "the file wins, even marked overridable")
	assert.False(t, res.Overridable)

	t.Setenv("KMS_PROVIDER", "aws")
	assert.Equal(t, "aws", r.Get("KMS_PROVIDER"))
}

// Where uploads and backups are stored is the operator's, so a value the admin
// layer holds for the storage keys never applies.
func TestResolver_StorageKeysIgnoreTheAdminLayer(t *testing.T) {
	r := NewResolver(map[string]FileValue{"STORAGE_S3_BUCKET": {Value: "uploads", Overridable: true}})
	r.SetAdminLayer(map[string]string{
		"storage_s3_bucket":   "elsewhere",
		"storage_s3_endpoint": "https://storage.example",
		"storage.s3.region":   "us-east-1",
	})

	assert.Equal(t, "uploads", r.Get("STORAGE_S3_BUCKET"), "the file wins, even marked overridable")
	assert.Equal(t, "", r.Get("STORAGE_S3_ENDPOINT"))
	assert.Equal(t, "", r.Get("STORAGE_S3_REGION"))
	assert.Equal(t, []string{"STORAGE_S3_BUCKET", "STORAGE_S3_ENDPOINT", "STORAGE_S3_REGION"}, r.IgnoredAdminKeys())
}
