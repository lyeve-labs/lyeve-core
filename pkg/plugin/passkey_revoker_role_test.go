package plugin

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// passkeyHolder is a plugin of any name that stores passkeys and removes
// count of them for any account.
type passkeyHolder struct {
	*fakePlugin
	count int
}

func (p *passkeyHolder) PasskeyRevoker() core.PasskeyRevoker { return p }

func (p *passkeyHolder) RevokePasskeys(context.Context, uuid.UUID) (int, error) {
	return p.count, nil
}

func TestPasskeyRevoker_NoImplementerIsNil(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("bystander"))

	assert.Nil(t, a.PasskeyRevoker())
}

func TestPasskeyRevoker_ComesFromThePluginThatImplementsIt(t *testing.T) {
	a := startRolePlugins(t, newFakePlugin("bystander"),
		&passkeyHolder{fakePlugin: newFakePlugin("any-name"), count: 2})

	r := a.PasskeyRevoker()
	require.NotNil(t, r)
	n, err := r.RevokePasskeys(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	require.NoError(t, a.Stop(context.Background()))
	assert.Nil(t, a.PasskeyRevoker(), "the role outlived the plugin that held it")
}

func TestPasskeyRevoker_TwoImplementersRefuseTheBoot(t *testing.T) {
	a := startRolePlugins(t,
		&passkeyHolder{fakePlugin: newFakePlugin("passkeys-b")},
		&passkeyHolder{fakePlugin: newFakePlugin("passkeys-a")},
	)

	err := a.CheckRoles()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "core.PasskeyRevokerProvider")
	assert.Nil(t, a.PasskeyRevoker(), "a role two plugins claim is wired from neither")
}
