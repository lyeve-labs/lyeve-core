// Command lyeve-core runs the engine on its own. No plugin is compiled in and
// no license verifier is linked, so it builds from this module alone. A build
// that wants plugins has a main of its own that blank-imports them.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lyeve-labs/lyeve-core/pkg/runtime"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	// The container HEALTHCHECK runs this binary again with this argument, and
	// the probe exits before anything boots.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		runtime.Healthcheck()
	}

	// JSON from the first line on. The runtime logs its own start line, with
	// the version, once its logging is configured.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The options name no license verifier, because this binary links none.
	// The engine then runs on licensing.Open, which verifies no license and
	// starts every plugin compiled in, and this binary compiles none.
	err := runtime.RunWithOptions(ctx, runtime.Options{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	})
	if err != nil {
		logger.Error("boot failed", "err", err)
		os.Exit(1)
	}
}
