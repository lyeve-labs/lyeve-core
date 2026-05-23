package plugin

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// hostKeeper keeps the host the activator starts it on, so a test can ask that
// host what it grants.
type hostKeeper struct {
	name string
	host core.Host
}

func (p *hostKeeper) Name() string { return p.name }

func (p *hostKeeper) Start(_ context.Context, host core.Host) error {
	p.host = host
	return nil
}

func (p *hostKeeper) Stop(context.Context) error { return nil }

// The boot audit prints AllPluginCaps, and an operator reads each line as what
// that plugin can reach. Each case starts one plugin through the activator and
// holds its report to the host the plugin was handed, one bit at a time,
// through the check every gated host method makes.
func TestAllPluginCaps_MatchesTheScopedHost(t *testing.T) {
	restoreCapTable(t)
	const listed = "cron"
	capPolicy := map[string]core.Capability{listed: core.CapDBWrite | core.CapRawDB | core.CapRoutes}
	SetCapPolicy(capPolicy)

	cases := []struct {
		name     string
		plugin   string
		declares bool
		declared core.Capability
		want     core.Capability
		explicit bool
	}{
		{
			name:     "plain registration with no policy entry is granted nothing",
			plugin:   "caps-report-unlisted",
			want:     0,
			explicit: false,
		},
		{
			name:     "plain registration with a policy entry is granted the entry",
			plugin:   listed,
			want:     capPolicy[listed],
			explicit: true,
		},
		{
			name:     "a declaration with no policy entry is the grant",
			plugin:   "caps-report-declared",
			declares: true,
			declared: core.CapDBRead | core.CapHooks,
			want:     core.CapDBRead | core.CapHooks,
			explicit: true,
		},
		{
			name:     "a declaration narrows the policy entry and never widens it",
			plugin:   listed,
			declares: true,
			declared: core.CapRoutes | core.CapConfigSecret,
			want:     capPolicy[listed] & (core.CapRoutes | core.CapConfigSecret),
			explicit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetPluginRegistry()
			t.Cleanup(resetPluginRegistry)

			p := &hostKeeper{name: tc.plugin}
			factory := func() core.Plugin { return p }
			if tc.declares {
				RegisterPluginWithCaps(tc.plugin, factory, tc.declared)
			} else {
				RegisterPlugin(tc.plugin, factory)
			}

			a := NewActivator(testHost{}, silentLogger)
			a.Resolve(grants(tc.plugin), "")
			require.NoError(t, a.Start(context.Background()))
			t.Cleanup(func() { _ = a.Stop(context.Background()) })
			require.NotNil(t, p.host, "the activator did not start the plugin")
			enforced, ok := p.host.(core.CapabilityChecker)
			require.True(t, ok, "the plugin was started on a host that enforces no capabilities")

			entries := AllPluginCaps()
			require.Len(t, entries, 1)
			got := entries[0]
			assert.Equal(t, tc.plugin, got.Name)
			assert.Equal(t, tc.explicit, got.Explicit, "Explicit")
			assert.Equal(t, tc.want, got.Caps, "reported caps")

			// Both sides are read through Has, where a write grant also
			// answers for read, so the two masks compare like for like.
			var held, reported core.Capability
			for bit := core.Capability(1); bit != 0; bit <<= 1 {
				if enforced.HasCapability(bit) {
					held |= bit
				}
				if got.Caps.Has(bit) {
					reported |= bit
				}
			}
			assert.Equal(t, held, reported, "the report disagrees with the scoped host")
		})
	}
}
