package enginehost

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// pluginSettings are settings plugins read that the engine has no field for:
// the mail relay, the search backend, the cache backend, the media transforms
// and the shared Redis. Each resolves through the layered resolver like any
// other plugin key, and the plugin that reads it applies its own default when
// no layer sets it.
var pluginSettings = []string{
	"smtp_host", "smtp_port", "smtp_from", "smtp_tls",
	"search_provider", "search_es_urls", "search_es_username", "search_es_index_prefix",
	"search_meili_url", "search_meili_index_prefix",
	"cache_driver", "storage_transform_driver", "storage_transform_url", "media_thumbnails",
}

// pluginCredentials are the credentials among them, which Config redacts and
// only Secret serves.
var pluginCredentials = []string{
	"smtp_user", "smtp_pass", "redis_url", "rate_limit_redis_url",
	"search_es_api_key", "search_es_password", "search_meili_api_key",
}

// unsetEnv removes a variable for one test and puts it back afterwards.
// t.Setenv cannot express this: a variable set to empty is present, and an
// empty variable pins a key rather than leaving it unset.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	if prev, ok := os.LookupEnv(name); ok {
		t.Cleanup(func() { _ = os.Setenv(name, prev) })
	} else {
		t.Cleanup(func() { _ = os.Unsetenv(name) })
	}
	_ = os.Unsetenv(name)
}

// A plugin reads the value an operator sees on the configuration page, from
// whichever layer supplies it. Unset, the host answers nothing and the plugin
// falls back to its own default. A value stored in the admin layer reaches
// the plugin, and a variable set in the deployment still wins over it.
func TestPluginSettings_ResolveThroughTheLayers(t *testing.T) {
	c := &configAdapter{cfg: &config.Config{}}
	h := &engineHost{cfg: &config.Config{}}
	t.Cleanup(func() { SetPluginConfigOverlay(nil) })

	for _, key := range pluginSettings {
		t.Run(key, func(t *testing.T) {
			name := upper(key)
			unsetEnv(t, name)
			SetPluginConfigOverlay(nil)
			assert.Empty(t, c.String(key), "unset, the plugin's own default applies")
			assert.Empty(t, c.Strings(key))

			SetPluginConfigOverlay(map[string]string{key: "stored"})
			assert.Equal(t, "stored", c.String(key), "the stored value reaches the plugin")

			t.Setenv(name, "from-env")
			assert.Equal(t, "from-env", c.String(key), "the deployment's variable wins")
		})
	}

	for _, key := range pluginCredentials {
		t.Run(key, func(t *testing.T) {
			name := upper(key)
			unsetEnv(t, name)
			SetPluginConfigOverlay(nil)
			_, found := h.Secret(key)
			assert.False(t, found, "unset, the plugin's own default applies")

			SetPluginConfigOverlay(map[string]string{key: "stored-secret"})
			val, found := h.Secret(key)
			assert.True(t, found)
			assert.Equal(t, "stored-secret", val, "the stored credential reaches the plugin")
			assert.Empty(t, c.String(key), "Config never serves a credential")

			t.Setenv(name, "env-secret")
			val, _ = h.Secret(key)
			assert.Equal(t, "env-secret", val, "the deployment's variable wins")
		})
	}
}

// A list setting reads the same whether it is split by the host or by the
// plugin: entries separated by commas, blanks dropped.
func TestPluginSettings_ListSettingSplitsOnCommas(t *testing.T) {
	c := &configAdapter{cfg: &config.Config{}}
	t.Setenv("SEARCH_ES_URLS", "http://es-1:9200, ,http://es-2:9200")
	assert.Equal(t, []string{"http://es-1:9200", "http://es-2:9200"}, c.Strings("search_es_urls"))
}
