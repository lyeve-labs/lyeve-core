package plugin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureActivator returns an activator logging into buf, so a boot summary can
// be read the way an operator reads it: from the log, without an authenticated
// admin request.
func captureActivator(t *testing.T) (*Activator, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewActivator(testHost{}, logger), &buf
}

// An optional plugin that dies is a feature down, not a broken install, so
// readiness deliberately still passes. Without a summary line that install
// would look healthy on every signal an operator has.
func TestStart_SummaryNamesFailedOptionalPlugins(t *testing.T) {
	always := ungatedNames(t, 1)
	plugins := withRegistered(t, append(always, "gamma")...)
	plugins["gamma"].startErr = errors.New("migrate: table already exists")

	a, buf := captureActivator(t)
	a.Resolve(ungated(always...).and(grants("gamma")), "")
	_ = a.Start(context.Background())

	out := buf.String()
	line := findLogLine(out, "plugins failed to start")
	if line == "" {
		t.Fatalf("no start summary logged; log was:\n%s", out)
	}
	if !strings.Contains(line, "gamma") {
		t.Errorf("summary does not name the failed plugin: %s", line)
	}
	if !strings.Contains(line, "failed=1") {
		t.Errorf("summary does not carry the failure count: %s", line)
	}
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("an optional plugin failing should be a warning, not an error: %s", line)
	}
	if a.AllReady() != nil {
		t.Errorf("a failed optional plugin must not fail readiness: %v", a.AllReady())
	}
}

// An ungated plugin failing is the case readiness also rejects, so the
// summary says so at Error and names which of them failed.
func TestStart_SummaryEscalatesForUngatedPlugins(t *testing.T) {
	names := ungatedNames(t, 2)
	failing := names[1]
	plugins := withRegistered(t, names...)
	plugins[failing].startErr = errors.New("migrate: relation does not exist")

	a, buf := captureActivator(t)
	a.Resolve(ungated(names...), "")
	_ = a.Start(context.Background())

	line := findLogLine(buf.String(), "plugins failed to start")
	if line == "" {
		t.Fatalf("no start summary logged; log was:\n%s", buf.String())
	}
	if !strings.Contains(line, "level=ERROR") {
		t.Errorf("an ungated plugin failing should be an error: %s", line)
	}
	if !strings.Contains(line, "ungated=["+failing+"]") {
		t.Errorf("summary does not identify the failed plugin as required: %s", line)
	}
}

// A clean boot reports the running count, so "no failures" is a positive signal
// rather than the absence of one.
func TestStart_SummaryReportsCleanBoot(t *testing.T) {
	names := ungatedNames(t, 2)
	withRegistered(t, names...)

	a, buf := captureActivator(t)
	a.Resolve(ungated(names...), "")
	_ = a.Start(context.Background())

	out := buf.String()
	if line := findLogLine(out, "plugins failed to start"); line != "" {
		t.Errorf("clean boot logged a failure summary: %s", line)
	}
	line := findLogLine(out, "all plugins started")
	if line == "" {
		t.Fatalf("clean boot logged no summary; log was:\n%s", out)
	}
	if !strings.Contains(line, "running=2") {
		t.Errorf("summary does not carry the running count: %s", line)
	}
}

// findLogLine returns the first line of out containing msg, or "".
func findLogLine(out, msg string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, msg) {
			return line
		}
	}
	return ""
}
