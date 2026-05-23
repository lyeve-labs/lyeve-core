package api

import (
	"slices"
	"sort"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// stubEntitlements is a hand-written EntitlementProvider for tests: a feature
// set, and the license state the snapshot reports. It withholds nothing.
type stubEntitlements struct {
	features map[string]bool
	// state is the license state. Empty means "active", so a test that only
	// cares about features says nothing about it.
	state string
}

func (s stubEntitlements) Snapshot() licensing.Snapshot {
	feats := []string{}
	for f, ok := range s.features {
		if ok {
			feats = append(feats, f)
		}
	}
	sort.Strings(feats)
	state := s.state
	if state == "" {
		state = "active"
	}
	return licensing.Snapshot{Plan: "test", State: state, Features: feats, Caps: map[string]int{}}
}

func (s stubEntitlements) Withholds(string, string) bool { return false }

func (s stubEntitlements) WithheldFrom(string) []string { return []string{} }

func (s stubEntitlements) Plugin(name string) licensing.PluginGrant {
	return licensing.PluginGrant{Start: s.features[name]}
}

func TestWithEntitlements_setsProvider(t *testing.T) {
	t.Parallel()

	stub := stubEntitlements{
		features: map[string]bool{"feature-b": true},
	}
	o := applyRouterOptions([]RouterOption{WithEntitlements(stub)})

	if o.entitlements == nil {
		t.Fatal("expected entitlements provider to be set")
	}
	if !slices.Contains(o.entitlements.Snapshot().Features, "feature-b") {
		t.Error("expected feature-b granted through the provider")
	}
}

func TestApplyRouterOptions_EntitlementsDefaultToUnlicensed(t *testing.T) {
	t.Parallel()

	o := applyRouterOptions(nil)
	if o.entitlements == nil {
		t.Fatal("expected a non-nil unlicensed provider by default (fail-closed)")
	}
	if slices.Contains(o.entitlements.Snapshot().Features, "feature-a") {
		t.Error("the unlicensed default must grant no licensed feature")
	}
}
