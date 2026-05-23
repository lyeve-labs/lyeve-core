package enginehost

import (
	"context"
	"testing"

	"github.com/lyeve-labs/lyeve-core/pkg/core"
)

type stubFlowValidator struct{}

func (stubFlowValidator) Validate(context.Context, string, []byte, string) ([]byte, []core.FlowProblem, error) {
	return nil, nil, nil
}

// The host answers nil until a plugin registers a validator, the very
// validator it was given afterwards, and nil again once the plugin registers
// nil on its way out, so a caller between a stop and the next start finds
// nothing rather than a validator whose plugin has gone.
func TestEngineHost_FlowDefinitionValidator_RegisteredByPlugin(t *testing.T) {
	h := &engineHost{}
	var provider core.FlowDefinitionValidatorProvider = h
	var registrar core.FlowDefinitionValidatorRegistrar = h
	if got := provider.FlowDefinitionValidator(); got != nil {
		t.Fatalf("FlowDefinitionValidator() before any registration = %v, want nil", got)
	}
	v := stubFlowValidator{}
	registrar.RegisterFlowDefinitionValidator(v)
	if got := provider.FlowDefinitionValidator(); got != core.FlowDefinitionValidator(v) {
		t.Errorf("FlowDefinitionValidator() = %v, want the registered validator", got)
	}
	registrar.RegisterFlowDefinitionValidator(nil)
	if got := provider.FlowDefinitionValidator(); got != nil {
		t.Errorf("FlowDefinitionValidator() after RegisterFlowDefinitionValidator(nil) = %v, want nil", got)
	}
}

type stubLocalizer struct{}

func (stubLocalizer) Localize(_ context.Context, _, _, _ string, data map[string]any) (map[string]any, string, error) {
	return data, "en", nil
}

// The localizer follows the same lifecycle as the validator: absent, then the
// registered one, then absent again, and a consumer's scoped host sees each
// step through the engine host.
func TestEngineHost_ContentLocalizer_RegisteredByPlugin(t *testing.T) {
	h := &engineHost{}
	var provider core.ContentLocalizerProvider = h
	var registrar core.ContentLocalizerRegistrar = h
	if got := provider.ContentLocalizer(); got != nil {
		t.Fatalf("ContentLocalizer() before any registration = %v, want nil", got)
	}
	l := stubLocalizer{}
	owner := core.NewScopedHost(h, "localization", core.CapDBWrite|core.CapRoutes)
	consumer := core.NewScopedHost(h, "graphql", core.CapRoutes)
	owner.RegisterContentLocalizer(l)
	if got := provider.ContentLocalizer(); got != core.ContentLocalizer(l) {
		t.Errorf("ContentLocalizer() = %v, want the registered localizer", got)
	}
	if got := consumer.ContentLocalizer(); got != core.ContentLocalizer(l) {
		t.Errorf("consumer ContentLocalizer() = %v, want the localizer the owner registered", got)
	}
	registrar.RegisterContentLocalizer(nil)
	if got := consumer.ContentLocalizer(); got != nil {
		t.Errorf("consumer ContentLocalizer() after the owner left = %v, want nil", got)
	}
}
