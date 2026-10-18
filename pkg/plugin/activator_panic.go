package plugin

import (
	"fmt"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// ReportPanic hands a panic the engine recovered to every running plugin that
// implements core.PanicReporter, in plugin name order. The runtime installs it
// as the engine's panic reporter, so the holders are read on each panic and a
// plugin that starts, stops or changes license is reached from the next one.
// A reporter that panics is logged and the rest are still told.
func (a *Activator) ReportPanic(p core.RecoveredPanic) {
	for _, np := range a.running() {
		r, ok := np.plugin.(core.PanicReporter)
		if !ok {
			continue
		}
		a.reportPanicTo(np.name, r, p)
	}
}

func (a *Activator) reportPanicTo(name string, r core.PanicReporter, p core.RecoveredPanic) {
	defer func() {
		if rp := recover(); rp != nil {
			a.logger.Error("plugin ReportPanic() panicked", "plugin", name, "panic", fmt.Sprintf("%v", rp))
		}
	}()
	r.ReportPanic(p)
}

var _ core.PanicReporter = (*Activator)(nil)
