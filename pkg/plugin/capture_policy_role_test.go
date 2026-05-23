package plugin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// policyHolder is a plugin of any name that supplies the capture policy. It
// imports nothing but pkg/core, which is all a third party needs to hold it.
type policyHolder struct {
	*fakePlugin
	ttl time.Duration
}

func (p *policyHolder) CapturePolicy() core.CapturePolicy { return p }

func (p *policyHolder) CaptureFor(_, _, path string) (time.Duration, bool) {
	return p.ttl, path != "/api/v1/health"
}

func TestCapturePolicy_NoImplementerIsNil(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("bystander"))

	assert.Nil(t, a.CapturePolicy())
}

func TestCapturePolicy_ComesFromThePluginThatImplementsIt(t *testing.T) {
	a := startRolePlugins(t, &policyHolder{fakePlugin: newFakePlugin("keeper"), ttl: 7 * 24 * time.Hour})

	policy := a.CapturePolicy()
	require.NotNil(t, policy)
	ttl, capture := policy.CaptureFor("acme", "GET", "/api/v1/posts")
	assert.True(t, capture)
	assert.Equal(t, 7*24*time.Hour, ttl)
	_, capture = policy.CaptureFor("acme", "GET", "/api/v1/health")
	assert.False(t, capture)
}

func TestCapturePolicy_TwoImplementersRefuseTheBoot(t *testing.T) {
	a := startRolePlugins(t,
		&policyHolder{fakePlugin: newFakePlugin("keeper-b")},
		&policyHolder{fakePlugin: newFakePlugin("keeper-a")},
	)

	err := a.CheckRoles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "core.CapturePolicyProvider")
	assert.Nil(t, a.CapturePolicy(), "a role two plugins claim is wired from neither")
}
