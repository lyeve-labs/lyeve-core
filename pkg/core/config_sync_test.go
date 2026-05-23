package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type namedSection string

func (s namedSection) ConfigSectionName() string { return string(s) }
func (s namedSection) ExportConfig(context.Context, ConfigOptions) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (s namedSection) PlanConfig(context.Context, json.RawMessage, ConfigOptions) (ConfigPlan, error) {
	return ConfigPlan{}, nil
}
func (s namedSection) ApplyConfig(context.Context, Querier, json.RawMessage, ConfigOptions) (ConfigApplied, error) {
	return ConfigApplied{}, nil
}

func sectionNames(ss []ConfigSection) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ConfigSectionName()
	}
	return out
}

func TestConfigSectionRegistry_SortsReplacesAndRemoves(t *testing.T) {
	var r ConfigSectionRegistry
	if got := r.ConfigSections(); len(got) != 0 {
		t.Fatalf("zero registry lists %v", sectionNames(got))
	}
	r.RegisterConfigSection("webhooks", namedSection("webhooks"))
	r.RegisterConfigSection("flows", namedSection("flows"))
	r.RegisterConfigSection("permissions", namedSection("permissions"))
	r.RegisterConfigSection("", namedSection("ignored"))

	got := sectionNames(r.ConfigSections())
	want := []string{"flows", "permissions", "webhooks"}
	if len(got) != len(want) {
		t.Fatalf("sections %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sections %v, want %v", got, want)
		}
	}

	r.RegisterConfigSection("flows", namedSection("flows-again"))
	if got := sectionNames(r.ConfigSections()); got[0] != "flows-again" {
		t.Fatalf("a second registration under one name did not replace the first: %v", got)
	}

	r.RegisterConfigSection("flows", nil)
	if got := sectionNames(r.ConfigSections()); len(got) != 2 || got[0] != "permissions" {
		t.Fatalf("a nil registration did not remove the name: %v", got)
	}
}

func TestScopedHost_ForwardsConfigSections(t *testing.T) {
	inner := &sectionHost{stubHost: &stubHost{}}
	owner := NewScopedHost(inner, "flow", CapDBRead)
	owner.RegisterConfigSection("flows", namedSection("flows"))
	if got := sectionNames(inner.ConfigSections()); len(got) != 1 || got[0] != "flows" {
		t.Fatalf("the registration did not reach the inner host: %v", got)
	}
	reader := NewScopedHost(inner, "schema", CapConfigSectionsRead)
	if got := sectionNames(reader.ConfigSections()); len(got) != 1 || got[0] != "flows" {
		t.Fatalf("scoped host with the grant lists %v, want the registered section", got)
	}
}

func TestScopedHost_ConfigSectionsDeniedWithoutTheGrant(t *testing.T) {
	inner := &sectionHost{stubHost: &stubHost{}}
	inner.RegisterConfigSection("webhooks", namedSection("webhooks"))
	h := NewScopedHost(inner, "flow", CapAll&^CapConfigSectionsRead)
	if got := h.ConfigSections(); got != nil {
		t.Fatalf("a plugin without CapConfigSectionsRead listed %v", sectionNames(got))
	}
}

func TestScopedHost_RefusesASectionAnotherPluginOwns(t *testing.T) {
	inner := &sectionHost{stubHost: &stubHost{}}
	NewScopedHost(inner, "webhook", CapDBRead).RegisterConfigSection("webhooks", namedSection("webhooks"))
	inner.RegisterConfigSection("settings", namedSection("settings"))

	intruder := NewScopedHost(inner, "flow", CapDBRead)
	intruder.RegisterConfigSection("webhooks", namedSection("forged"))
	intruder.RegisterConfigSection("webhooks", nil)
	intruder.RegisterConfigSection("settings", namedSection("forged"))

	got := sectionNames(inner.ConfigSections())
	if len(got) != 2 || got[0] != "settings" || got[1] != "webhooks" {
		t.Fatalf("sections after another plugin wrote over them: %v", got)
	}
}

func TestConfigSectionRegistry_OwnerKeepsItsName(t *testing.T) {
	var r ConfigSectionRegistry
	if err := r.RegisterOwnedConfigSection("webhook", "webhooks", namedSection("webhooks")); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterOwnedConfigSection("flow", "webhooks", namedSection("forged")); !errors.Is(err, ErrConfigSectionOwned) {
		t.Fatalf("a second owner's registration: %v, want ErrConfigSectionOwned", err)
	}
	if err := r.RegisterOwnedConfigSection("flow", "webhooks", nil); !errors.Is(err, ErrConfigSectionOwned) {
		t.Fatalf("a second owner's removal: %v, want ErrConfigSectionOwned", err)
	}
	if err := r.RegisterOwnedConfigSection("webhook", "webhooks", namedSection("webhooks-again")); err != nil {
		t.Fatalf("the owner could not replace its own section: %v", err)
	}
	if err := r.RegisterOwnedConfigSection("webhook", "webhooks", nil); err != nil {
		t.Fatalf("the owner could not remove its own section: %v", err)
	}
	if err := r.RegisterOwnedConfigSection("flow", "webhooks", namedSection("flows")); err != nil {
		t.Fatalf("a released name stayed held: %v", err)
	}
}

func TestScopedHost_NoRegistryListsNothing(t *testing.T) {
	h := NewScopedHost(&stubHost{}, "schema", CapConfigSectionsRead)
	h.RegisterConfigSection("flows", namedSection("flows"))
	if got := h.ConfigSections(); got != nil {
		t.Fatalf("a host without a registry listed %v", sectionNames(got))
	}
}

func TestCapConfigSectionsRead_IsNamedAndInCapAll(t *testing.T) {
	if !CapAll.Has(CapConfigSectionsRead) {
		t.Error("CapAll must include CapConfigSectionsRead")
	}
	if capName(CapConfigSectionsRead) != "CapConfigSectionsRead" {
		t.Errorf("capName = %q", capName(CapConfigSectionsRead))
	}
}

func TestConfigApplyError_UnwrapsToCause(t *testing.T) {
	cause := errors.New("duplicate key")
	err := error(&ConfigApplyError{Key: "nightly", Err: cause})
	if !errors.Is(err, cause) {
		t.Fatal("ConfigApplyError does not unwrap to its cause")
	}
	var ae *ConfigApplyError
	if !errors.As(err, &ae) || ae.Key != "nightly" {
		t.Fatalf("errors.As lost the key: %v", err)
	}
}

type sectionHost struct {
	*stubHost
	ConfigSectionRegistry
}
