// Package compliance holds the security controls report in the shape the
// boot log and strict mode read it.
//
// Each control says whether it is genuinely enforcing, not just compiled in,
// which catches a control plugin that ships in the binary with its
// enforcement never wired. The plugin that supplies a control reports it.
// This package logs the report and formats it, so a control that is
// configured but not enforcing is in the log before the first request.
package compliance

import (
	"context"
	"fmt"
	"log/slog"
)

// ControlStatus represents the liveness status of a single control.
type ControlStatus string

const (
	StatusPass ControlStatus = "PASS" // compiled, configured, and enforcing
	StatusWarn ControlStatus = "WARN" // compiled but not configured (acceptable)
	StatusFail ControlStatus = "FAIL" // configured/compiled but NOT enforcing
	StatusSkip ControlStatus = "SKIP" // not compiled in or not applicable
)

// CheckResult is the outcome of a single control liveness assertion.
type CheckResult struct {
	Control string        `json:"control"`
	Status  ControlStatus `json:"status"`
	Detail  string        `json:"detail"`
}

// ControlReport is the full set of liveness assertions run at boot.
type ControlReport struct {
	Checks []CheckResult `json:"checks"`
}

// HasFailures returns true if any check returned StatusFail.
func (r ControlReport) HasFailures() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Failures returns only the failed checks.
func (r ControlReport) Failures() []CheckResult {
	var out []CheckResult
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			out = append(out, c)
		}
	}
	return out
}

// Helpers

// LogReport logs each control check at the appropriate level.
// FAIL -> Error, WARN -> Warn, PASS/DEBUG -> Info.
// Returns true if any check failed (for strict-mode fail-fast).
func LogReport(logger *slog.Logger, report ControlReport) bool {
	hasFailures := false
	for _, c := range report.Checks {
		attrs := []slog.Attr{
			slog.String("control", c.Control),
			slog.String("status", string(c.Status)),
			slog.String("detail", c.Detail),
		}
		switch c.Status {
		case StatusFail:
			hasFailures = true
			logger.LogAttrs(context.Background(), slog.LevelError, "control liveness: FAIL", attrs...)
		case StatusWarn:
			logger.LogAttrs(context.Background(), slog.LevelWarn, "control liveness: WARN", attrs...)
		default:
			logger.LogAttrs(context.Background(), slog.LevelInfo, "control liveness: "+string(c.Status), attrs...)
		}
	}
	if hasFailures {
		logger.Error("control liveness: one or more controls FAILED - see above")
	} else {
		logger.Info("control liveness: all controls passed or skipped")
	}
	return hasFailures
}

// FormatReport returns a human-readable summary suitable for CI output.
func FormatReport(report ControlReport) string {
	s := "=== Security Control Liveness Report ===\n"
	for _, c := range report.Checks {
		s += fmt.Sprintf("  [%s] %-20s %s\n", c.Status, c.Control, c.Detail)
	}
	if report.HasFailures() {
		s += "\nRESULT: FAIL - one or more controls not enforced\n"
	} else {
		s += "\nRESULT: PASS\n"
	}
	return s
}
