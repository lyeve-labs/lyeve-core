package runtime

import (
	"github.com/lyeve-labs/lyeve-core/internal/compliance"
	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/plugin"
)

// securityControls answers the admin's /security/controls from what the
// compiled plugins report, evaluated at request time rather than copied at
// boot: a plugin the license activates later is reported as it is now, not
// as it was.
type securityControls struct {
	cfg       *config.Config
	activator *plugin.Activator
	// order is Options.ControlOrder.
	order []string
}

// SecurityControls returns every row a compiled plugin reports for itself
// through core.SecurityControlReporter, in the order the build names its
// controls. A control no compiled plugin supplies has no row.
func (s securityControls) SecurityControls() []core.SecurityControl {
	return orderControls(s.activator.ReportControls(s.env()), s.order)
}

// orderControls lists the rows of each control order names, in that order,
// then the rows of every other control, in the order each was first
// reported. The rows of one control stay together, in the order they were
// reported.
func orderControls(rows []core.SecurityControl, order []string) []core.SecurityControl {
	byControl := make(map[string][]core.SecurityControl, len(rows))
	var reported []string
	for _, r := range rows {
		if _, seen := byControl[r.Control]; !seen {
			reported = append(reported, r.Control)
		}
		byControl[r.Control] = append(byControl[r.Control], r)
	}

	out := make([]core.SecurityControl, 0, len(rows))
	for _, name := range order {
		out = append(out, byControl[name]...)
		delete(byControl, name)
	}
	for _, name := range reported {
		out = append(out, byControl[name]...)
	}
	return out
}

// report is SecurityControls in the shape the boot log and strict mode read.
func (s securityControls) report() compliance.ControlReport {
	rows := s.SecurityControls()
	checks := make([]compliance.CheckResult, len(rows))
	for i, r := range rows {
		checks[i] = compliance.CheckResult{Control: r.Control, Status: compliance.ControlStatus(r.Status), Detail: r.Detail}
	}
	return compliance.ControlReport{Checks: checks}
}

// env is what a reporter is told about the install. The activator adds,
// per plugin, whether it runs and which of its roles are wired.
func (s securityControls) env() core.ControlEnv {
	return core.ControlEnv{
		InstanceRegion:  s.cfg.InstanceRegion,
		GlobalRateLimit: s.cfg.RateLimitRPS > 0,
		TrustedProxies:  len(s.cfg.TrustedProxies) > 0,
		MFALockout:      true, // AuthHandler always holds an MFA lockout, in-process or shared
	}
}
