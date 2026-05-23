package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/internal/testdb"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// Saving engine settings.
//
// The !overridable tag and the editable flag the provenance endpoint reports
// are only meaningful if something can act on them, so these cover the write
// side: which keys are accepted, which are refused and why, and that a save
// leaves settings the same screen did not touch alone.

// withResolver installs a resolver for one test and restores the previous one,
// because the active resolver is process-wide.
func withResolver(t *testing.T, files map[string]config.FileValue) {
	t.Helper()
	prev := config.SetActiveResolver(config.NewResolver(files))
	t.Cleanup(func() { config.SetActiveResolver(prev) })
}

func saveSettings(t *testing.T, store *db.PluginConfigStore, body string) (*httptest.ResponseRecorder, configSaveResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/config", strings.NewReader(body))
	req = req.WithContext(core.WithTenantID(req.Context(), "default"))
	configSaveHandler(store, nil).ServeHTTP(rec, req)

	var out configSaveResponse
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func configStore(t *testing.T) *db.PluginConfigStore {
	t.Helper()
	return db.NewPluginConfigStore(testdb.Postgres(t)).
		WithSealer(db.NewConfigSealer("test-master-key-for-config-settings"))
}

func TestConfigSave_StoresAnOverridableFileKey(t *testing.T) {
	withResolver(t, map[string]config.FileValue{
		"STORAGE_DRIVER": {Value: "local", Overridable: true, Origin: "lyeve.yaml:21"},
	})
	store := configStore(t)

	rec, out := saveSettings(t, store, `{"values":{"storage_driver":"s3"}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, out.Saved, 1)
	assert.Equal(t, "STORAGE_DRIVER", out.Saved[0].Key)
	assert.Empty(t, out.Refused)
}

func TestConfigSave_RefusesAKeyTheEnvironmentHolds(t *testing.T) {
	// The environment always wins, so storing the key would leave the operator
	// looking at a saved value the engine never reads.
	t.Setenv("RATE_LIMIT_RPS", "777")
	withResolver(t, map[string]config.FileValue{
		"RATE_LIMIT_RPS": {Value: "250", Overridable: true, Origin: "lyeve.yaml:19"},
	})
	store := configStore(t)

	rec, out := saveSettings(t, store, `{"values":{"rate_limit_rps":"100"}}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Len(t, out.Refused, 1)
	assert.Equal(t, "RATE_LIMIT_RPS", out.Refused[0].Key)
	assert.Contains(t, out.Refused[0].Reason, "environment variable")
	assert.Empty(t, out.Saved)
}

func TestConfigSave_RefusesAKeyThePinnedFileHolds(t *testing.T) {
	withResolver(t, map[string]config.FileValue{
		"APP_ENV": {Value: "production", Origin: "lyeve.yaml:33"},
	})
	store := configStore(t)

	rec, out := saveSettings(t, store, `{"values":{"app_env":"development"}}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Len(t, out.Refused, 1)
	assert.Contains(t, out.Refused[0].Reason, "!overridable")
	assert.Equal(t, "lyeve.yaml:33", out.Refused[0].Origin,
		"the refusal names the file holding the key, or the operator cannot find it")
}

func TestConfigSave_StoresTheAcceptedKeysAndReportsTheRest(t *testing.T) {
	// A partial save is better than none: the screen has one pinned field and
	// refusing the whole submission would make the other fields uneditable.
	t.Setenv("APP_ENV", "production")
	withResolver(t, map[string]config.FileValue{
		"STORAGE_DRIVER": {Value: "local", Overridable: true, Origin: "lyeve.yaml:21"},
	})
	store := configStore(t)

	rec, out := saveSettings(t, store, `{"values":{"storage_driver":"s3","app_env":"development"}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, out.Saved, 1)
	assert.Equal(t, "STORAGE_DRIVER", out.Saved[0].Key)
	require.Len(t, out.Refused, 1)
	assert.Equal(t, "APP_ENV", out.Refused[0].Key)
}

func TestConfigSave_LeavesUntouchedSettingsAlone(t *testing.T) {
	// Set writes the whole row, so a second save must merge rather than replace.
	withResolver(t, map[string]config.FileValue{})
	store := configStore(t)

	rec, _ := saveSettings(t, store, `{"values":{"first_setting":"one"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	rec, _ = saveSettings(t, store, `{"values":{"second_setting":"two"}}`)
	require.Equal(t, http.StatusOK, rec.Code)

	stored, err := store.Get(core.WithTenantID(t.Context(), "default"), config.CoreSettingsOwner)
	require.NoError(t, err)
	assert.Equal(t, "one", stored["FIRST_SETTING"], "the first save survived the second")
	assert.Equal(t, "two", stored["SECOND_SETTING"])
}

func TestConfigSave_StoredCredentialsAreNeverReturned(t *testing.T) {
	withResolver(t, map[string]config.FileValue{})
	store := configStore(t)

	rec, out := saveSettings(t, store, `{"values":{"provider_api_key":"sk-live-do-not-disclose"}}`)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, out.Saved, 1)
	assert.True(t, out.Saved[0].Secret, "the _key suffix names it a credential")
	assert.Empty(t, out.Saved[0].Value)
	assert.NotContains(t, rec.Body.String(), "sk-live-do-not-disclose")

	stored, err := store.Get(core.WithTenantID(t.Context(), "default"), config.CoreSettingsOwner)
	require.NoError(t, err)
	assert.Equal(t, db.SecretMask, stored["PROVIDER_API_KEY"], "a read gives back the mask")
}

func TestConfigSave_RejectsABodyThatIsNotASettingsObject(t *testing.T) {
	withResolver(t, map[string]config.FileValue{})
	store := configStore(t)

	for name, body := range map[string]string{
		"no values field": `{}`,
		"empty values":    `{"values":{}}`,
		"not an object":   `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := saveSettings(t, store, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestConfigSave_WithoutAStoreIsUnavailable(t *testing.T) {
	rec, _ := saveSettings(t, nil, `{"values":{"storage_driver":"s3"}}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// A setting the engine describes carries its description and default, so the
// page that edits it says what it does. One it does not describe carries none.
func TestSettingResponse_CarriesTheSettingsDescription(t *testing.T) {
	got := settingResponse(config.Resolution{Key: "JWT_EXPIRY_SECS", Value: "900", From: config.SourceEnv})
	assert.Contains(t, got.Description, "Access token lifetime")
	assert.Equal(t, "900 (15 minutes)", got.Default)

	got = settingResponse(config.Resolution{Key: "SOME_PLUGIN_KEY", Value: "x", From: config.SourceAdmin})
	assert.Empty(t, got.Description)
	assert.Empty(t, got.Default)
}

func TestConfigSave_RefusesAnOperatorOnlyKey(t *testing.T) {
	// Where backups are encrypted is the operator's to decide. A stolen admin
	// session must not be able to repoint it.
	withResolver(t, nil)
	store := configStore(t)

	rec, out := saveSettings(t, store, `{"values":{"kms_endpoint":"https://attacker.example"}}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	require.Len(t, out.Refused, 1)
	assert.Equal(t, "KMS_ENDPOINT", out.Refused[0].Key)
	assert.Contains(t, out.Refused[0].Reason, "only by the operator")
	assert.Empty(t, out.Saved)
}

type schemaFor map[string]map[string]any

func (s schemaFor) PluginSchema(name string) map[string]any { return s[name] }

func TestPluginConfigSave_RefusesAnOperatorOnlyKey(t *testing.T) {
	withResolver(t, nil)
	store := configStore(t)
	schemas := schemaFor{"example": {"type": "object"}}

	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/plugins/example/config", strings.NewReader(body))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", "example")
		req = req.WithContext(context.WithValue(core.WithTenantID(req.Context(), "default"), chi.RouteCtxKey, rctx))
		pluginConfigSaveHandler(store, schemas, nil).ServeHTTP(rec, req)
		return rec
	}

	rec := put(`{"kms_provider":"local"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "only by the operator")

	rec = put(`{"storage_s3_backup_allowed_buckets":"attacker-bucket"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	// Every spelling the resolver folds to the same name is refused too.
	for _, key := range []string{"kms-provider", "KMS.Provider", "webhook_allowed_private_networks"} {
		rec = put(`{"` + key + `":"x"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, key)
	}

	// Ordinary plugin settings still save.
	rec = put(`{"example_retention_days":30}`)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
