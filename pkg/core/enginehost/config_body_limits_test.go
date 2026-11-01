package enginehost

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/internal/config"
	"github.com/lyeve-labs/lyeve-core/internal/db"
)

type storedSettings map[string]any

func (s storedSettings) ListAll(context.Context) ([]db.PluginConfigRow, error) {
	return []db.PluginConfigRow{{PluginName: config.CoreSettingsOwner, Config: s}}, nil
}

// bodyLimitsAfterBoot reads both body limits the way boot does, then installs
// stored values that differ from what the engine applies, the state a failed
// apply of the stored settings leaves behind.
func bodyLimitsAfterBoot(t *testing.T) (*config.Resolver, *configAdapter) {
	t.Helper()
	r := config.NewResolver(nil)
	prev := config.SetActiveResolver(r)
	t.Cleanup(func() { config.SetActiveResolver(prev) })

	r.Get("MAX_JSON_BODY_BYTES")
	r.Get("MAX_BODY_BYTES")
	_, err := config.RefreshAdminLayer(context.Background(), storedSettings{
		"MAX_JSON_BODY_BYTES": "65536",
		"MAX_BODY_BYTES":      "20971520",
	})
	require.NoError(t, err)
	return r, &configAdapter{cfg: &config.Config{MaxJSONBodyBytes: 1 << 20, MaxBodyBytes: 10 << 20}}
}

// A plugin that reads the body limits when it starts sees the values the
// engine enforces, not a stored value the engine has not applied.
func TestConfigAdapter_BodyLimitsAreTheOnesTheEngineApplies(t *testing.T) {
	_, c := bodyLimitsAfterBoot(t)

	assert.Equal(t, "1048576", c.String("max_json_body_bytes"))
	assert.Equal(t, "10485760", c.String("max_body_bytes"))
}

// The engine applies the body limits only at boot. A plugin reading them when
// it starts must leave them reported as waiting for the next start, or the
// console calls a saved limit live while nothing applies it.
func TestConfigAdapter_BodyLimitsReadByAPluginStayReadAtStart(t *testing.T) {
	r, c := bodyLimitsAfterBoot(t)

	c.String("max_json_body_bytes")
	c.String("max_body_bytes")

	assert.True(t, r.Resolve("MAX_JSON_BODY_BYTES").ReadAtStart)
	assert.True(t, r.Resolve("MAX_BODY_BYTES").ReadAtStart)
}
