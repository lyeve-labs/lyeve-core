package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The report lists the controls a build names in the order it names them,
// whichever plugin reported each and in whatever order, so a console that
// reads the rows in order sees them in that order.
func TestOrderControls_ListsTheNamedControlsInTheirOrder(t *testing.T) {
	pii := core.SecurityControl{Control: "pii-mask", Status: core.ControlSkip, Detail: "plugin: pii"}
	wafA := core.SecurityControl{Control: "waf", Status: core.ControlPass, Detail: "plugin: waf rules"}
	wafB := core.SecurityControl{Control: "waf", Status: core.ControlWarn, Detail: "plugin: waf mode"}
	mfa := core.SecurityControl{Control: "mfa", Status: core.ControlPass, Detail: "plugin: mfa"}
	honeypot := core.SecurityControl{Control: "honeypot", Status: core.ControlPass, Detail: "plugin: honeypot"}
	canary := core.SecurityControl{Control: "canary", Status: core.ControlFail, Detail: "plugin: canary"}
	order := []string{"pii-mask", "waf", "mfa"}

	for _, tc := range []struct {
		name     string
		order    []string
		reported []core.SecurityControl
		want     []core.SecurityControl
	}{
		{
			name:  "nothing reported",
			order: order,
		},
		{
			name:     "the named controls in the order named",
			order:    order,
			reported: []core.SecurityControl{mfa, wafA, pii},
			want:     []core.SecurityControl{pii, wafA, mfa},
		},
		{
			name:     "every row of one control together, in the order reported",
			order:    order,
			reported: []core.SecurityControl{wafA, honeypot, wafB, pii},
			want:     []core.SecurityControl{pii, wafA, wafB, honeypot},
		},
		{
			name:     "a control the order does not name follows, in the order first reported",
			order:    order,
			reported: []core.SecurityControl{honeypot, mfa, canary},
			want:     []core.SecurityControl{mfa, honeypot, canary},
		},
		{
			name:     "a named control nothing reports adds no row",
			order:    []string{"widgets", "mfa"},
			reported: []core.SecurityControl{mfa},
			want:     []core.SecurityControl{mfa},
		},
		{
			name:     "no order keeps the order reported",
			reported: []core.SecurityControl{wafA, mfa, wafB},
			want:     []core.SecurityControl{wafA, wafB, mfa},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := orderControls(tc.reported, tc.order)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
