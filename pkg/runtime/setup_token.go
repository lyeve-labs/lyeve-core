package runtime

import (
	"context"
	"fmt"
	"io"
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
// random token is generated and written once to the console: reading the
// process's own output proves access to the host, which is the principal
// setup should trust. The token never goes through slog, because the log
// chain also feeds the stored log, the in-process rings and any external
// sink, and a reader of those is not that principal. An install that already
// has accounts gets no token, and neither does one whose count fails, so setup
// stays closed rather than guessing.
func firstRunSetupToken(ctx context.Context, cfg *config.Config, users userCounter, logger *slog.Logger, console io.Writer) *api.SetupToken {
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
	_, _ = fmt.Fprintf(console, "setup_token=%s\n", plain) // err suppressed: without a console, LYEVE_SETUP_TOKEN is the way in
	logger.Warn("setup: no account exists yet. A one-time setup token was written to this process's standard error, outside the log. "+
		"Enter it on the admin's first-run page. It works until the first super admin is created. "+
		"With more than one replica, set LYEVE_SETUP_TOKEN instead, since each replica prints its own.",
		"instance_id", cfg.InstanceID)
	return tok
}
