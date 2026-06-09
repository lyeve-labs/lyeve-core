package logging

import "github.com/lyeve-labs/lyeve-core/pkg/observability"

// init points the setters in pkg/observability at the global enrich handler,
// so a plugin reaches them without importing this package, which would
// cycle. Registration only: it assigns function pointers and nothing else.
func init() {
	observability.SetGlobalLogLeveler = func(leveler observability.LogLevelManager) {
		if h := GlobalEnrichHandler(); h != nil {
			h.SetLeveler(leveler)
		}
	}

	observability.SetGlobalLogSink = func(sink observability.LogSink) {
		if h := GlobalEnrichHandler(); h != nil {
			h.SetSink(sink)
		}
	}
}
