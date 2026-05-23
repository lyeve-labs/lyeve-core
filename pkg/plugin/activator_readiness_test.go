package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAllReady_FailedPluginIsNotReady covers a required plugin that failed: a
// plugin its grant marks ungated whose Start returned an error sits in
// PhaseFailed, and AllReady has to count it. Otherwise /readyz would answer
// 200 for an install with a dead required plugin, and an orchestrator would
// route traffic to it.
func TestAllReady_FailedPluginIsNotReady(t *testing.T) {
	names := ungatedNames(t, 2)
	failing := names[1]
	plugins := withRegistered(t, names...)
	plugins[failing].startErr = errors.New("migrate: relation example_items does not exist")

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(names...), "")
	_ = a.Start(context.Background())

	err := a.AllReady()
	if err == nil {
		t.Fatalf("AllReady returned nil with %s in PhaseFailed", failing)
	}
	if !strings.Contains(err.Error(), failing) {
		t.Errorf("AllReady error does not name the failed plugin: %v", err)
	}
	if !strings.Contains(err.Error(), "example_items") {
		t.Errorf("AllReady error drops the start failure cause: %v", err)
	}
}

// TestAllReady_FailedOptionalPluginDoesNotBlockReadiness covers the other side
// of the split. A granted plugin its grant does not mark ungated that fails to
// start is one feature down, not a broken install. Failing readiness on it
// would make every install whose license names a plugin with an unmet
// dependency answer /readyz 503 on every boot, so an orchestrator would send
// it no traffic at all. The failure still has to be visible in Status.
func TestAllReady_FailedOptionalPluginDoesNotBlockReadiness(t *testing.T) {
	always := ungatedNames(t, 2)
	plugins := withRegistered(t, append(always, "gamma")...)
	plugins["gamma"].startErr = errors.New("migrate: relation example_items does not exist")

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(always...).and(grants("gamma")), "")
	_ = a.Start(context.Background())

	if err := a.AllReady(); err != nil {
		t.Errorf("AllReady = %v; want nil when only a gated plugin failed", err)
	}

	var reported bool
	for _, s := range a.Status().Plugins {
		if s.Name == "gamma" && s.Phase == PhaseFailed {
			reported = true
		}
	}
	if !reported {
		t.Error("gamma start failure is absent from Status; it must stay visible")
	}
}

// TestAllReady_AllRunningIsReady guards against over-reporting: an install
// whose plugins all started must stay ready.
func TestAllReady_AllRunningIsReady(t *testing.T) {
	names := ungatedNames(t, 2)
	withRegistered(t, names...)

	a := NewActivator(testHost{}, nil)
	a.Resolve(ungated(names...), "")
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := a.AllReady(); err != nil {
		t.Errorf("AllReady = %v; want nil with every plugin running", err)
	}
}
