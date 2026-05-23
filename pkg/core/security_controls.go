package core

// Security controls a plugin reports for itself.
//
// The admin's security controls report says, for each control an install can
// run, whether it is enforcing. The plugin that supplies a control is the one
// that knows what enforcing means for it, so it reports its own rows. The
// engine hands it the facts only the engine has: whether the plugin runs,
// which of its roles the engine wired, and the settings that decide the
// answer.

// ControlStatus is how one control stands on this install.
type ControlStatus string

const (
	// ControlPass is a control that is running, configured and enforcing.
	ControlPass ControlStatus = "PASS"
	// ControlWarn is a control that runs but is not configured, which is
	// allowed.
	ControlWarn ControlStatus = "WARN"
	// ControlFail is a control that is configured but not enforcing.
	ControlFail ControlStatus = "FAIL"
	// ControlSkip is a control that does not run on this install, or does not
	// apply to it.
	ControlSkip ControlStatus = "SKIP"
)

// SecurityControl is one row of the security controls report.
type SecurityControl struct {
	// Control names the control, and stays the same across releases, because
	// a console labels the row by it.
	Control string        `json:"control"`
	Status  ControlStatus `json:"status"`
	// Detail says why the control has its status.
	Detail string `json:"detail"`
	// Remedy is the one thing an operator does to make the control enforce.
	// The report shows it only on a row that is not passing.
	Remedy string `json:"remedy,omitempty"`
}

// ControlEnv is what the engine tells a reporter about the install.
type ControlEnv struct {
	// Running is whether the reporting plugin runs. A compiled plugin that
	// does not run is asked too, so its rows read SKIP rather than vanish.
	Running bool
	// Wired says, for each role the plugin implements, whether the engine
	// wired the plugin's implementation of it. It is keyed by the role's
	// interface as Go prints it, such as "core.PIIMiddlewareProvider".
	Wired map[string]bool
	// InstanceRegion is INSTANCE_REGION, empty when it is not set.
	InstanceRegion string
	// GlobalRateLimit is whether the engine's own per-address limiter is on.
	GlobalRateLimit bool
	// TrustedProxies is whether TRUSTED_PROXIES names any proxy.
	TrustedProxies bool
	// MFALockout is whether the engine counts failed MFA attempts per user.
	MFALockout bool
}

// SecurityControlReporter is implemented by a plugin that reports the
// security controls it supplies. The engine asks every compiled plugin that
// implements it, in plugin name order, whether it runs or not.
type SecurityControlReporter interface {
	SecurityControls(env ControlEnv) []SecurityControl
}
