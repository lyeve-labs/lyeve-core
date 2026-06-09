package compliance

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FormatReport

func TestFormatReport_AllPass(t *testing.T) {
	report := ControlReport{
		Checks: []CheckResult{
			{"waf", StatusPass, "waf middleware active"},
			{"mfa", StatusPass, "mfa store + per-user lockout active"},
		},
	}
	out := FormatReport(report)
	assert.Contains(t, out, "[PASS]")
	assert.Contains(t, out, "RESULT: PASS")
	assert.NotContains(t, out, "FAIL")
}

func TestFormatReport_WithFailure(t *testing.T) {
	report := ControlReport{
		Checks: []CheckResult{
			{"waf", StatusPass, "ok"},
			{"pii-mask", StatusFail, "response-masking middleware not in active chain"},
		},
	}
	out := FormatReport(report)
	assert.Contains(t, out, "[FAIL]")
	assert.Contains(t, out, "RESULT: FAIL")
}

// LogReport

func TestLogReport_ReturnsTrueOnFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	report := ControlReport{
		Checks: []CheckResult{
			{"waf", StatusPass, "ok"},
			{"pii-mask", StatusFail, "missing middleware"},
		},
	}
	hasFailures := LogReport(logger, report)
	assert.True(t, hasFailures)
	assert.Contains(t, buf.String(), "FAIL")
}

func TestLogReport_ReturnsFalseWhenAllPass(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	report := ControlReport{
		Checks: []CheckResult{
			{"waf", StatusPass, "ok"},
			{"mfa", StatusSkip, "not active"},
		},
	}
	hasFailures := LogReport(logger, report)
	assert.False(t, hasFailures)
	assert.Contains(t, buf.String(), "all controls passed")
}

// HasFailures / Failures

func TestHasFailures(t *testing.T) {
	tests := []struct {
		name   string
		checks []CheckResult
		want   bool
	}{
		{"empty", nil, false},
		{"all pass", []CheckResult{{"a", StatusPass, ""}}, false},
		{"has fail", []CheckResult{{"a", StatusPass, ""}, {"b", StatusFail, ""}}, true},
		{"warn only", []CheckResult{{"a", StatusWarn, ""}}, false},
		{"skip only", []CheckResult{{"a", StatusSkip, ""}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ControlReport{Checks: tt.checks}
			assert.Equal(t, tt.want, r.HasFailures())
			if tt.want {
				require.NotEmpty(t, r.Failures())
				for _, f := range r.Failures() {
					assert.Equal(t, StatusFail, f.Status)
				}
			}
		})
	}
}
