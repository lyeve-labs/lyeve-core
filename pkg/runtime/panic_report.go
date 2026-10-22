package runtime

import (
	"github.com/lyeve-labs/lyeve-core/internal/panicreport"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// reportPanicsTo returns the engine's panic reporter, which hands each
// recovered panic to r. The conversion fails to compile when the two panic
// types drift apart.
func reportPanicsTo(r core.PanicReporter) panicreport.Reporter {
	return func(p panicreport.Panic) { r.ReportPanic(core.RecoveredPanic(p)) }
}
