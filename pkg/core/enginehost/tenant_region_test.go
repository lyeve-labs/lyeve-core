package enginehost

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type fixedRegion string

func (r fixedRegion) TenantRegion(context.Context, string) (string, bool, error) {
	return string(r), true, nil
}

// regionSource stands in for the activator: what it answers changes as the
// plugin holding the role starts and stops.
type regionSource struct{ current core.TenantRegionResolver }

func (s *regionSource) TenantRegionResolver() core.TenantRegionResolver { return s.current }

// The host answers nil until the runtime wires a source, and asks the source
// on every call afterwards, so a holder that starts late or stops is seen by
// the next caller.
func TestEngineHost_TenantRegionResolver_AsksTheSourceOnEveryCall(t *testing.T) {
	h := &engineHost{}
	var host core.TenantRegionResolverProvider = h
	if got := host.TenantRegionResolver(); got != nil {
		t.Fatalf("TenantRegionResolver() before wiring = %v, want nil", got)
	}

	src := &regionSource{}
	h.WithTenantRegions(src)
	if got := host.TenantRegionResolver(); got != nil {
		t.Fatalf("TenantRegionResolver() with no holder = %v, want nil", got)
	}

	src.current = fixedRegion("eu-west-1")
	got := host.TenantRegionResolver()
	if got == nil {
		t.Fatal("TenantRegionResolver() = nil after the holder started")
	}
	if region, assigned, err := got.TenantRegion(context.Background(), "acme"); err != nil || !assigned || region != "eu-west-1" {
		t.Errorf("TenantRegion() = %q, %v, %v, want eu-west-1, true, nil", region, assigned, err)
	}

	src.current = nil
	if got := host.TenantRegionResolver(); got != nil {
		t.Errorf("TenantRegionResolver() after the holder stopped = %v, want nil", got)
	}
}
