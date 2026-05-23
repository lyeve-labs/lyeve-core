package enginehost

import (
	"strconv"
	"time"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// Plugin-declared configuration.
//
// configAdapter answers a closed switch of keys core itself owns. A plugin
// option core never enumerated must not be silently inert: a plugin that read
// its key and got "" or false or 0 would behave as though the operator had
// chosen that.
//
// The route below closes the gap without core having to enumerate every
// plugin's options. An unknown key resolves through the layered configuration
// resolver under its upper-cased name, so example_keyspace reads
// EXAMPLE_KEYSPACE from the environment, the YAML tree, or the admin layer, in
// that order.
//
// This grants a plugin nothing it did not already have. Plugins are Go
// packages compiled into the same binary and can read the environment
// directly. Routing through the host only makes the lookup uniform
// and testable. The redaction boundary is what matters, and it is enforced by
// core.IsSecretKey on the Config path: credential material stays reachable only
// through SecretsProvider.

// SetPluginConfigOverlay installs the operator-set configuration that plugin
// keys fall back to.
//
// Safe to call on a running engine: the layer is swapped atomically, so a
// plugin that reads its configuration per request picks up the new value
// without a restart. A plugin that caches configuration in Start observes the
// change only if it implements core.Reloadable.
//
// The environment and the YAML tree still take precedence. An operator who
// pins a value in the deployment expects it to hold, so a stored value fills
// gaps rather than overriding the container's own configuration, unless the
// YAML key marks itself !overridable.
func SetPluginConfigOverlay(values map[string]string) {
	config.ActiveResolver().SetAdminLayer(values)
}

// pluginEnv resolves an unknown config key through the layered resolver.
// Returns "" when no layer has it.
func pluginEnv(key string) string {
	if key == "" {
		return ""
	}
	return config.ActiveResolver().Get(key)
}

// pluginEnvBool parses an unknown boolean key. Anything unparseable is false
// rather than failing a boot on a typo.
func pluginEnvBool(key string) bool {
	v, err := strconv.ParseBool(pluginEnv(key))
	return err == nil && v
}

// pluginEnvDuration parses an unknown duration key. Accepts Go duration syntax
// ("30s", "5m"). A bare integer is rejected rather than guessed at a unit.
func pluginEnvDuration(key string) time.Duration {
	d, err := time.ParseDuration(pluginEnv(key))
	if err != nil {
		return 0
	}
	return d
}
