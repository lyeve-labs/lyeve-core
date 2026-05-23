package core

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

type fixedGatherer struct{ families []*dto.MetricFamily }

func (g fixedGatherer) Gather() ([]*dto.MetricFamily, error) { return g.families, nil }

type metricsHost struct {
	*stubHost
	g MetricsGatherer
}

func (h *metricsHost) MetricsGatherer() MetricsGatherer { return h.g }

type capturingMetricsHost struct {
	*capturingHost
	g MetricsGatherer
}

func (h *capturingMetricsHost) MetricsGatherer() MetricsGatherer { return h.g }

// A granted plugin reads the registry the inner host holds, unwrapped, and
// what it reads satisfies the Prometheus gatherer shape.
func TestScopedHost_MetricsGatherer_ForwardsWhenGranted(t *testing.T) {
	name := "lyeve_requests_total"
	g := fixedGatherer{families: []*dto.MetricFamily{{Name: &name}}}
	sh := NewScopedHost(&metricsHost{stubHost: &stubHost{}, g: g}, "telemetry", CapMetrics)

	var host Host = sh
	p, ok := host.(MetricsGathererProvider)
	if !ok {
		t.Fatal("ScopedHost must satisfy MetricsGathererProvider through the Host interface")
	}
	got := p.MetricsGatherer()
	if got == nil {
		t.Fatal("MetricsGatherer() = nil, want the inner host's registry")
	}
	fams, err := got.Gather()
	if err != nil || len(fams) != 1 || fams[0].GetName() != name {
		t.Errorf("Gather() = %v, %v; want the one family through", fams, err)
	}
}

// Without the grant the plugin gets nil, and the denial is in the log with
// the grant to add.
func TestScopedHost_MetricsGatherer_DeniedIsNilAndLogged(t *testing.T) {
	h := newCapturingHost()
	sh := NewScopedHost(&capturingMetricsHost{capturingHost: h, g: fixedGatherer{}}, "telemetry", CapRoutes|CapConfigSecret)

	if got := sh.MetricsGatherer(); got != nil {
		t.Fatalf("MetricsGatherer() = %v, want nil when denied", got)
	}
	sh.MetricsGatherer()

	rec := requireOneDenial(t, h)
	assertField(t, rec, "op", "MetricsGatherer")
	assertField(t, rec, "capability", "CapMetrics")
	assertField(t, rec, "plugin", "telemetry")
}

// A host built outside the engine has no registry to hand over. That is
// unavailable, not denied.
func TestScopedHost_MetricsGatherer_NilWhenInnerHasNone(t *testing.T) {
	sh := NewScopedHost(&stubHost{}, "telemetry", CapAll)
	if got := sh.MetricsGatherer(); got != nil {
		t.Errorf("MetricsGatherer() = %v, want nil when the inner host has no registry", got)
	}
}

func TestCapAll_GrantsMetrics(t *testing.T) {
	t.Parallel()
	if !CapAll.Has(CapMetrics) {
		t.Error("CapAll must include CapMetrics")
	}
	if capName(CapMetrics) != "CapMetrics" {
		t.Errorf("capName = %q", capName(CapMetrics))
	}
}
