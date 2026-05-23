package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// validatorHost is a stubHost that can hold a flow definition validator, the
// way the engine host does once the plugin that runs flows has registered
// one.
type validatorHost struct {
	*stubHost
	validator FlowDefinitionValidator
}

func (h *validatorHost) RegisterFlowDefinitionValidator(v FlowDefinitionValidator) {
	h.validator = v
}
func (h *validatorHost) FlowDefinitionValidator() FlowDefinitionValidator { return h.validator }

// unavailableRecords returns every "plugin host service unavailable" record
// the host logged.
func unavailableRecords(t *testing.T, h *capturingHost) []logRecord {
	t.Helper()
	var out []logRecord
	for _, line := range bytes.Split([]byte(h.buf.String()), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec logRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line is not JSON: %s (%v)", line, err)
		}
		if rec["msg"] == "plugin host service unavailable" {
			out = append(out, rec)
		}
	}
	return out
}

// fakeValidator answers a fixed verdict and records what it was asked.
type fakeValidator struct {
	tenants []string
	formats []string
}

func (f *fakeValidator) Validate(ctx context.Context, tenantID string, def []byte, format string) ([]byte, []FlowProblem, error) {
	f.tenants = append(f.tenants, tenantID)
	f.formats = append(f.formats, format)
	if len(def) == 0 {
		return nil, nil, errors.New("empty definition")
	}
	return []byte(`{"nodes":[]}`), []FlowProblem{{NodeID: "n1", Path: "/config/prompt", Message: "required"}}, nil
}

// The plugin that runs flows registers its validator through its host and
// the plugin that drafts definitions fetches the same one through its own.
func TestScopedHost_FlowDefinitionValidator_RegisteredAndFetched(t *testing.T) {
	inner := &validatorHost{stubHost: &stubHost{}}
	flow := NewScopedHost(inner, "flow", CapAll)
	ai := NewScopedHost(inner, "ai", CapDBWrite|CapRoutes)

	v := &fakeValidator{}
	flow.RegisterFlowDefinitionValidator(v)

	var host Host = ai
	p, ok := host.(FlowDefinitionValidatorProvider)
	if !ok {
		t.Fatal("ScopedHost must satisfy FlowDefinitionValidatorProvider through the Host interface")
	}
	got := p.FlowDefinitionValidator()
	if got != FlowDefinitionValidator(v) {
		t.Fatalf("FlowDefinitionValidator() = %T, want the registered validator", got)
	}
	norm, problems, err := got.Validate(context.Background(), "acme", []byte("name: x"), "yaml")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if string(norm) != `{"nodes":[]}` || len(problems) != 1 || problems[0].Path != "/config/prompt" {
		t.Errorf("verdict did not pass through: %s %+v", norm, problems)
	}
	if v.tenants[0] != "acme" || v.formats[0] != "yaml" {
		t.Errorf("arguments did not pass through: %v %v", v.tenants, v.formats)
	}

	flow.RegisterFlowDefinitionValidator(nil)
	if ai.FlowDefinitionValidator() != nil {
		t.Error("after RegisterFlowDefinitionValidator(nil) the validator must be nil")
	}
}

// A bare host answers nil and logs once, with the op named so the line says
// which service was asked for.
func TestScopedHost_FlowDefinitionValidator_NilWhenInnerCannotProvide(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(h, "ai", CapAll)
	for range 3 {
		if got := sh.FlowDefinitionValidator(); got != nil {
			t.Fatalf("FlowDefinitionValidator() = %v, want nil", got)
		}
	}
	recs := unavailableRecords(t, h)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 unavailable log record, got %d: %q", len(recs), h.buf.String())
	}
	assertField(t, recs[0], "op", "FlowDefinitionValidator")
}
