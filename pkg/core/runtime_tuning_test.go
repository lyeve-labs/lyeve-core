package core

import (
	"context"
	"errors"
	"testing"
)

// fixedTuning answers one view, so a test can tell it came through.
type fixedTuning struct{ view RuntimeSettingsView }

func (f fixedTuning) RuntimeFacts(context.Context) RuntimeFacts    { return RuntimeFacts{} }
func (f fixedTuning) RuntimeSettings() RuntimeSettingsView         { return f.view }
func (f fixedTuning) SetCPULimit(int) (RuntimeSettingsView, error) { return f.view, nil }
func (f fixedTuning) SetMemoryLimit(MemoryLimit) (RuntimeSettingsView, error) {
	return f.view, nil
}
func (f fixedTuning) SetGCPercent(int) (RuntimeSettingsView, error)        { return f.view, nil }
func (f fixedTuning) SetDatabaseMaxConns(int) (RuntimeSettingsView, error) { return f.view, nil }

type tuningHost struct {
	*stubHost
	tuning RuntimeTuning
}

func (h *tuningHost) RuntimeTuning() RuntimeTuning { return h.tuning }

// A plugin reaches the host's runtime settings through its scoped host, and a
// host built without them answers nil, so the plugin can report them as
// unavailable rather than serve a view that holds nothing.
func TestScopedHost_RuntimeTuning(t *testing.T) {
	want := RuntimeSettingsView{CPU: Setting{Value: 3, Mode: CPUFixed, Source: SourceSet}}
	tests := []struct {
		name  string
		inner Host
		want  bool
	}{
		{name: "forwards the inner host's role", inner: &tuningHost{stubHost: &stubHost{}, tuning: fixedTuning{view: want}}, want: true},
		{name: "nil from a host without the role", inner: &stubHost{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var host Host = NewScopedHost(tt.inner, "tuning", 0)
			p, ok := host.(RuntimeTuningProvider)
			if !ok {
				t.Fatal("ScopedHost must satisfy RuntimeTuningProvider through the Host interface")
			}
			got := p.RuntimeTuning()
			if (got != nil) != tt.want {
				t.Fatalf("RuntimeTuning() = %v, want present %v", got, tt.want)
			}
			if got != nil && got.RuntimeSettings().CPU != want.CPU {
				t.Errorf("RuntimeSettings().CPU = %+v, want %+v", got.RuntimeSettings().CPU, want.CPU)
			}
		})
	}
}

// A caller tells a pinned value from a bound by the sentinel, and shows the
// variable the error names.
func TestPinnedError_NamesTheVariable(t *testing.T) {
	err := error(&PinnedError{Variable: "GOMAXPROCS"})
	if !errors.Is(err, ErrSettingPinned) {
		t.Errorf("errors.Is(%v, ErrSettingPinned) = false", err)
	}
	var pinned *PinnedError
	if !errors.As(err, &pinned) || pinned.Variable != "GOMAXPROCS" {
		t.Errorf("errors.As gave %+v, want the variable GOMAXPROCS", pinned)
	}
}
