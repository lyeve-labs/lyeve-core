package runtime

import (
	"fmt"
	"os"
)

// Version answers the version flag and exits 0 when arg is --version,
// -version or version, printing one line with the version, commit and build
// date. Any other argument returns, and the caller boots. A main calls it
// before the engine boots, so asking the version never opens a database or
// binds a port.
func Version(arg, version, commit, buildDate string) {
	switch arg {
	case "--version", "-version", "version":
		fmt.Printf("lyeve %s (commit %s, built %s)\n", version, commit, buildDate)
		os.Exit(0)
	}
}

// Usage refuses an argument that Healthcheck, Migrate and Version all left
// unanswered. It prints the usage line to stderr and exits 2, so a mistyped
// flag never boots a second engine beside the running one.
func Usage(arg string) {
	fmt.Fprintf(os.Stderr, "lyeve: unknown argument %q\nusage: lyeve [healthcheck | --version | migrate mark-applied --plugin <name> --version <version>]\n", arg)
	os.Exit(2)
}
