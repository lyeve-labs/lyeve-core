package enginehost

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

// The runtime publishes the engine's rate limits for every build of the
// routers, while requests read the list from the one before. A reader gets a
// whole list, and a copy it may change without changing the host's.
func TestEngineHost_EngineRateLimits_ReplacedWhileRead(t *testing.T) {
	h := &engineHost{}
	if h.EngineRateLimits() != nil {
		t.Fatal("EngineRateLimits() must be nil before the first build")
	}
	list := func(n int) []core.EngineRateLimit {
		out := make([]core.EngineRateLimit, n)
		for i := range out {
			out[i] = core.EngineRateLimit{Scope: "public", Endpoint: "*", Rate: float64(n), Burst: n}
		}
		return out
	}
	h.WithEngineRateLimits(list(1))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 2; n < 200; n++ {
			h.WithEngineRateLimits(list(n))
		}
	}()
	for {
		got := h.EngineRateLimits()
		for _, l := range got {
			if l.Burst != len(got) {
				t.Fatalf("read a list of %d holding a row from a list of %d", len(got), l.Burst)
			}
		}
		select {
		case <-done:
			got := h.EngineRateLimits()
			if len(got) != 199 {
				t.Fatalf("EngineRateLimits() = %d rows after the last build, want 199", len(got))
			}
			got[0].Rate = -1
			if h.EngineRateLimits()[0].Rate == -1 {
				t.Fatal("a caller's change reached the host's list")
			}
			return
		default:
		}
	}
}
