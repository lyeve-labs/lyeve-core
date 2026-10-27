package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/db"
)

type rowsStore []db.PluginConfigRow

func (s rowsStore) ListAll(context.Context) ([]db.PluginConfigRow, error) { return s, nil }

// Load reads the token lifetime before the stored layer is installed, so a
// value saved for it later never reaches the running engine. The resolver
// must say so instead of offering the key as editable.
func TestResolver_KeyReadOnlyAtStartIsNotEditable(t *testing.T) {
	r := NewResolver(nil)
	prev := SetActiveResolver(r)
	t.Cleanup(func() { SetActiveResolver(prev) })

	r.Get("JWT_EXPIRY_SECS")
	_, err := RefreshAdminLayer(context.Background(), rowsStore{
		{PluginName: CoreSettingsOwner, Config: map[string]any{"JWT_EXPIRY_SECS": "1800"}},
	})
	require.NoError(t, err)

	res := r.Resolve("JWT_EXPIRY_SECS")
	assert.False(t, res.Overridable, "a save would not change what the engine uses")
	assert.True(t, res.ReadAtStart)
	assert.Equal(t, SourceNone, res.From, "the stored value is not the one in effect")
	assert.Equal(t, "", res.Value)
}

func TestResolver_KeyReadAgainAfterTheStoredLayerStaysEditable(t *testing.T) {
	r := NewResolver(nil)
	prev := SetActiveResolver(r)
	t.Cleanup(func() { SetActiveResolver(prev) })

	r.Get("EXAMPLE_HOST")
	_, err := RefreshAdminLayer(context.Background(), rowsStore{
		{PluginName: CoreSettingsOwner, Config: map[string]any{"EXAMPLE_HOST": "mail.example"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "mail.example", r.Get("EXAMPLE_HOST"))
	res := r.Resolve("EXAMPLE_HOST")
	assert.True(t, res.Overridable)
	assert.False(t, res.ReadAtStart)
}

// A deployment that opened the key with LYEVE_OVERRIDABLE still cannot change
// it from the admin layer once the engine has read it at start.
func TestResolver_KeyReadAtStartIgnoresTheEnvironmentOptIn(t *testing.T) {
	t.Setenv(OverridableEnvKey, "JWT_EXPIRY_SECS")
	t.Setenv("JWT_EXPIRY_SECS", "600")
	r := NewResolver(nil)

	r.Get("JWT_EXPIRY_SECS")
	r.SetAdminLayer(map[string]string{"JWT_EXPIRY_SECS": "1800"})

	res := r.Resolve("JWT_EXPIRY_SECS")
	assert.Equal(t, "600", res.Value)
	assert.Equal(t, SourceEnv, res.From)
	assert.False(t, res.Overridable)
	assert.True(t, res.ReadAtStart)
}
