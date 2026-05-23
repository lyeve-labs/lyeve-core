package runtime

import (
	"context"
	"log/slog"

	"github.com/lyeve-labs/lyeve-core/internal/api"
	"github.com/lyeve-labs/lyeve-core/internal/config"
)

// userCounter is the one store call the boot needs to decide whether first-run
// setup is still open.
type userCounter interface {
	Count(ctx context.Context) (int64, error)
}

// firstRunSetupToken builds the process's setup token once, before the first
// router, so a router rebuilt by hot reload keeps it.
//
// LYEVE_SETUP_TOKEN wins when set. Otherwise, while no account exists, a
// random token is generated and printed once: reading the log proves access to
// the host, which is the principal setup should trust. An install that already
// has accounts gets no token, and neither does one whose count fails, so setup
// stays closed rather than guessing.
func firstRunSetupToken(ctx context.Context, cfg *config.Config, users userCounter, logger *slog.Logger) *api.SetupToken {
	if cfg.SetupToken != "" {
		return api.NewSetupTokenFromEnv(cfg.SetupToken)
	}
	n, err := users.Count(ctx)
	if err != nil {
		logger.Warn("setup: could not count accounts; first-run setup stays closed until restart", "error", err)
		return api.NewSetupTokenFromEnv("")
	}
	if n > 0 {
		return api.NewSetupTokenFromEnv("")
	}
	tok, plain, err := api.GenerateSetupToken()
	if err != nil {
		logger.Error("setup: could not generate a setup token; set LYEVE_SETUP_TOKEN", "error", err)
		return api.NewSetupTokenFromEnv("")
	}
	logger.Warn("setup: no account exists yet. Enter this setup token on the admin's first-run page. "+
		"It works until the first super admin is created. With more than one replica, set LYEVE_SETUP_TOKEN instead, "+
		"since each replica prints its own.",
		"setup_token", plain, "instance_id", cfg.InstanceID)
	return tok
}
