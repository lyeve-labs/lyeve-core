package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// A plugin refuses a capability the license does not grant with a sentinel of
// its own that wraps core.ErrNotGranted, and both mappers answer 402 however
// deep the wrapping goes. A store error that merely mentions the words is not
// the sentinel and keeps its own status.
func TestStatusFor_NotGrantedIsPaymentRequired(t *testing.T) {
	errFlowNotGranted := fmt.Errorf("flow: %w", core.ErrNotGranted)
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "the sentinel", err: core.ErrNotGranted, want: http.StatusPaymentRequired},
		{name: "a plugin sentinel wrapping it", err: errFlowNotGranted, want: http.StatusPaymentRequired},
		{name: "wrapped again on the way out", err: fmt.Errorf("invoke: %w", errFlowNotGranted), want: http.StatusPaymentRequired},
		{name: "the same words and no sentinel", err: errors.New("capability not granted"), want: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, StatusFor(tc.err))
			if tc.want == http.StatusPaymentRequired {
				assert.Equal(t, tc.want, StoreStatusFor(tc.err))
			}
		})
	}
}
