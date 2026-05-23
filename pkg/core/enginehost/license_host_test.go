package enginehost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
	"github.com/lyeve-labs/lyeve-core/pkg/licensing"
)

// ceiling is a named capacity ceiling a licensing implementation might state.
// The engine reads it through CapabilitySet.Limit and gives it no meaning.
const ceiling = "example.items"

// A host with no manager wired has no license to read, so it grants nothing,
// and both halves of core.LicenseHost say so.
func TestCapabilities_NoManagerGrantsNothing(t *testing.T) {
	h := &engineHost{}

	caps := h.Capabilities()
	assert.Equal(t, "free", caps.Plan)
	assert.Equal(t, "free", caps.State)
	assert.NotNil(t, caps.Features)
	assert.Empty(t, caps.Features)
	assert.False(t, h.HasFeature(context.Background(), "feature-a"))
}

// fakeLicensing is a licensing manager that reports one fixed snapshot and
// withholds from each tenant the names withheld lists for it. What the
// snapshot is worth, and what falls with a withheld name, is the licensing
// implementation's decision, so the host is covered for what it does with
// whatever answer it gets.
type fakeLicensing struct {
	licensing.Manager
	snap     licensing.Snapshot
	withheld map[string][]string
}

func (m fakeLicensing) Snapshot() licensing.Snapshot { return m.snap }

func (m fakeLicensing) Withholds(tenant, name string) bool {
	for _, n := range m.withheld[tenant] {
		if n == name {
			return true
		}
	}
	return false
}

// The host hands every plugin gate the snapshot as it is: the features, the
// plan and state, the tenant ceiling and the named ceilings. It adds nothing
// and drops nothing, and a plugin that changes the copy it was given cannot
// change what the next caller reads.
func TestCapabilities_CopiesTheManagersSnapshot(t *testing.T) {
	h := &engineHost{}
	h.WithLicensing(fakeLicensing{snap: licensing.Snapshot{
		Plan:        "plan-b",
		State:       "active",
		Features:    []string{"feature-a", "widgets-export"},
		TenantQuota: 5,
		Caps:        map[string]int{ceiling: 0},
	}})

	caps := h.Capabilities()
	assert.Equal(t, map[string]bool{"feature-a": true, "widgets-export": true}, caps.Features, "nothing beyond what the manager granted")
	assert.Equal(t, "plan-b", caps.Plan)
	assert.Equal(t, "active", caps.State)
	assert.Equal(t, 5, caps.TenantQuota)
	assert.Equal(t, 0, caps.Limit(ceiling))

	caps.Features["example-other"] = true
	caps.Caps[ceiling] = 3
	again := h.Capabilities()
	assert.False(t, again.Features["example-other"])
	assert.Equal(t, 0, again.Limit(ceiling))
}

// HasFeature answers from the same snapshot as Capabilities, so the two halves
// of core.LicenseHost cannot disagree, whatever the manager grants.
func TestHasFeature_AgreesWithTheManagersSnapshot(t *testing.T) {
	h := &engineHost{}
	h.WithLicensing(fakeLicensing{snap: licensing.Snapshot{
		Plan: "plan-b", State: "active", Features: []string{"feature-a", "widgets-export"},
	}})

	ctx := context.Background()
	caps := h.Capabilities()
	for _, name := range []string{"feature-a", "widgets-export", "example-other", "content"} {
		assert.Equalf(t, caps.Features[name], h.HasFeature(ctx, name), "the two halves disagree on %q", name)
	}
}

// A name the manager withholds from the request's tenant is refused whatever
// the snapshot grants the instance, and a call with no tenant on the context,
// such as a background job, is answered for the instance.
func TestHasFeature_RefusesWhatTheTenantIsWithheld(t *testing.T) {
	h := &engineHost{}
	h.WithLicensing(fakeLicensing{
		snap:     licensing.Snapshot{Plan: "plan-a", State: "free", Features: []string{"content", "widgets-store"}},
		withheld: map[string][]string{"acme": {"content", "widgets-store"}},
	})
	acme := core.WithTenantID(context.Background(), "acme")
	globex := core.WithTenantID(context.Background(), "globex")

	assert.False(t, h.HasFeature(acme, "content"))
	assert.False(t, h.HasFeature(acme, "widgets-store"))
	assert.True(t, h.HasFeature(globex, "content"))
	assert.True(t, h.HasFeature(context.Background(), "content"), "a call with no tenant answers for the instance")
}
