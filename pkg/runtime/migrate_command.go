package runtime

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// Migrate runs "lyeve migrate" and exits with its status when args[0] is
// migrate. Any other argument returns, and the caller goes on. A main calls it
// before Usage, which refuses every argument nothing before it answered.
func Migrate(args []string) {
	if len(args) > 0 && args[0] == "migrate" {
		os.Exit(MigrateCommand(args[1:], os.Stdout, os.Stderr))
	}
}

const migrateUsage = "usage: lyeve migrate mark-applied --plugin <name> --version <version>"

// MigrateCommand runs "lyeve migrate", the operator's repair for a plugin's
// migration bookkeeping, and returns the exit status. Marking a version
// applied skips its script, so the command has no HTTP route: only someone
// with a shell on the engine's host can run it, and a stolen admin session
// cannot. It connects to the database the engine's configuration names and
// prints neither the connection string nor any secret.
func MigrateCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "mark-applied" {
		_, _ = fmt.Fprintln(stderr, migrateUsage)
		return 2
	}
	flags := flag.NewFlagSet("mark-applied", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("plugin", "", "the plugin whose migration to mark")
	version := flags.String("version", "", "the migration version to mark applied, as the boot failure names it")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if *name == "" || *version == "" || flags.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, migrateUsage)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mark-applied: load configuration: %v\n", err)
		return 1
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mark-applied: connect to the database: %v\n", err)
		return 1
	}
	defer func() { _ = pool.Close() }()

	table, err := plugin.MarkPluginMigrationApplied(ctx, pool.SQLDB(), pool.Engine(), *name, *version)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mark-applied: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "marked %s applied for plugin %s in %s, without running its script\n", *version, *name, table)
	return 0
}
