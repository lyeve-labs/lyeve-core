package enginehost

import (
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type ownsAll struct{}

func (ownsAll) Owns(string) bool { return true }

func TestEngineHost_APIRoutes_RegisteredAndCleared(t *testing.T) {
	h := &engineHost{}
	if h.APIRoutes() != nil {
		t.Fatal("APIRoutes() must be nil before registration")
	}
	var r core.APIRoutes = ownsAll{}
	h.RegisterAPIRoutes(r)
	if got := h.APIRoutes(); got == nil || !got.Owns("/api/v1/x") {
		t.Fatalf("APIRoutes() = %v, want the registered answer", got)
	}
	h.RegisterAPIRoutes(nil)
	if h.APIRoutes() != nil {
		t.Fatal("RegisterAPIRoutes(nil) must clear the answer")
	}
}
