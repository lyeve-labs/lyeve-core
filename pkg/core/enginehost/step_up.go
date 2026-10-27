package enginehost

import (
	"fmt"
	"net/http"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// errStepUpUnwired is the answer before the admin router has handed the host
// its checks. It is an outage the caller may retry, never a refused answer.
var errStepUpUnwired = fmt.Errorf("step-up checks are not available yet: %w", core.ErrServiceUnavailable)

type stepUpBox struct{ checker core.StepUpChecker }

// SetStepUpChecker wires the engine's own step-up checks, the ones its token
// routes use, so a plugin's checks count against the same lockouts. Nil
// clears it.
func (h *engineHost) SetStepUpChecker(c core.StepUpChecker) {
	h.stepUps.Store(&stepUpBox{checker: c})
}

// CheckStepUp implements core.StepUpChecker.
func (h *engineHost) CheckStepUp(r *http.Request, factor, answer string) error {
	if b := h.stepUps.Load(); b != nil && b.checker != nil {
		return b.checker.CheckStepUp(r, factor, answer)
	}
	return errStepUpUnwired
}
