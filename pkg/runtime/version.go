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
