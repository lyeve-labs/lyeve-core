package runtime

import (
	"context"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// loadPluginConfigOverlay installs the operator-set configuration that the
// admin config editor writes to sys_plugin_config.
//
// A failure here is not fatal. The engine boots on the environment and the
// configuration file alone.
func loadPluginConfigOverlay(ctx context.Context, store config.AdminConfigStore, logger *slog.Logger) {
	layer, err := config.RefreshAdminLayer(ctx, store)
	if err != nil {
		logger.Warn("operator configuration unavailable - continuing on the environment and configuration file", "err", err)
		return
	}

	for _, conflict := range layer.Conflicts {
		logger.Warn("configuration key claimed by more than one plugin - last one wins", "conflict", conflict)
	}
	// A credential that will not decrypt is withheld rather than passed on as
	// ciphertext, so the plugin behaves as though it were never set. Naming it
	// here is the only signal an operator gets that a key rotation orphaned it.
	if len(layer.Undecryptable) > 0 {
		logger.Error("stored credentials could not be decrypted and are being ignored - re-enter them in the admin UI",
			"keys", layer.Undecryptable)
	}

	resolver := config.ActiveResolver()
	// Settings the file or the admin UI supplies that an empty variable
	// suppresses. This is how a deployment moving off .env silently keeps its
	// old behavior: godotenv loads the leftover blank names into the process.
	if blanked := resolver.BlankedKeys(); len(blanked) > 0 {
		logger.Warn("settings suppressed by empty environment variables", "keys", blanked)
	}
	// Operator-only settings stored through the admin API are ignored. Move
	// them to the environment or the configuration file to keep them.
	if ignored := resolver.IgnoredAdminKeys(); len(ignored) > 0 {
		logger.Error("settings stored through the admin API are ignored because only the operator may set them - move them to the environment or the configuration file",
			"keys", ignored)
	}

	if len(layer.Values) > 0 || resolver.FileKeys() > 0 {
		logger.Info("configuration layers loaded",
			"file_keys", resolver.FileKeys(),
			"admin_keys", len(layer.Values))
	}
}
