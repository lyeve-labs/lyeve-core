package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestDBCollector_CollectsAllMetrics(t *testing.T) {
	ResetForTesting()

	r := Init()
	if r == nil {
		t.Fatal("Init() returned nil registry")
	}

	c := NewDBCollector(func() DBStats {
		return DBStats{
			MaxOpenConnections: 100,
			OpenConnections:    42,
			InUse:              8,
			Idle:               34,
			WaitCount:          150,
			WaitDuration:       12.5,
			MaxIdleClosed:      1200,
			MaxIdleTimeClosed:  800,
			MaxLifetimeClosed:  50,
		}
	})
	r.MustRegister(c)

	// Record at least one request so the metric vectors produce output.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	RecordRequest(req, 200, 0.001)

	h := Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rr.Body.String()

	tests := []struct {
		metric string
	}{
		{"lyeve_db_pool_max_connections 100"},
		{"lyeve_db_pool_open_connections 42"},
		{"lyeve_db_pool_in_use_connections 8"},
		{"lyeve_db_pool_idle_connections 34"},
		{"lyeve_db_pool_wait_count_total 150"},
		{"lyeve_db_pool_wait_duration_seconds_total 12.5"},
		{"lyeve_db_pool_max_idle_closed_total 1200"},
		{"lyeve_db_pool_max_idle_time_closed_total 800"},
		{"lyeve_db_pool_max_lifetime_closed_total 50"},
	}

	for _, tt := range tests {
		if !strings.Contains(body, tt.metric) {
			t.Errorf("missing metric %q in output:\n%s", tt.metric, body)
		}
	}
}

func TestDBCollector_DescribeFillsChannel(t *testing.T) {
	c := NewDBCollector(func() DBStats { return DBStats{} })

	ch := make(chan *prometheus.Desc, 25)
	go func() {
		c.Describe(ch)
		close(ch)
	}()

	count := 0
	for range ch {
		count++
	}
	if count != 11 {
		t.Errorf("Describe sent %d descriptors, want 11", count)
	}
}

func TestDBCollector_NilSafeDescribe(t *testing.T) {
	c := NewDBCollector(func() DBStats { return DBStats{} })
	descs := make([]*prometheus.Desc, 0)
	ch := make(chan *prometheus.Desc, 20)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	for d := range ch {
		descs = append(descs, d)
	}
	if len(descs) != 11 {
		t.Errorf("got %d descriptors, want 11", len(descs))
	}
}

func TestDBCollector_ZeroStats(t *testing.T) {
	c := NewDBCollector(func() DBStats { return DBStats{} })

	ch := make(chan prometheus.Metric, 20)
	c.Collect(ch)
	close(ch)

	count := 0
	for m := range ch {
		_ = m
		count++
	}
	if count != 11 {
		t.Errorf("Collect sent %d metrics, want 11", count)
	}
}

// Utilization + exhaustion warning metrics

func TestDBCollector_UtilizationRatio(t *testing.T) {
	ResetForTesting()
	r := Init()

	c := NewDBCollector(func() DBStats {
		return DBStats{
			MaxOpenConnections: 100,
			InUse:              50, // 50% utilization
		}
	})
	r.MustRegister(c)

	h := Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "lyeve_db_pool_utilization_ratio 0.5") {
		t.Errorf("expected utilization_ratio 0.5, got:\n%s", body)
	}
	// 50% < 80% threshold -> no warning
	if !strings.Contains(body, "lyeve_db_pool_exhaustion_warning 0") {
		t.Errorf("expected exhaustion_warning 0, got:\n%s", body)
	}
}

func TestDBCollector_ExhaustionWarning_Triggered(t *testing.T) {
	ResetForTesting()
	r := Init()

	c := NewDBCollector(func() DBStats {
		return DBStats{
			MaxOpenConnections: 100,
			InUse:              85, // 85% > 80% threshold
		}
	})
	r.MustRegister(c)

	h := Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "lyeve_db_pool_utilization_ratio 0.85") {
		t.Errorf("expected utilization_ratio 0.85, got:\n%s", body)
	}
	if !strings.Contains(body, "lyeve_db_pool_exhaustion_warning 1") {
		t.Errorf("expected exhaustion_warning 1, got:\n%s", body)
	}
}

func TestDBCollector_Utilization_ZeroMaxOpen(t *testing.T) {
	c := NewDBCollector(func() DBStats {
		return DBStats{MaxOpenConnections: 0, InUse: 0}
	})

	ch := make(chan prometheus.Metric, 20)
	c.Collect(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	if count != 11 {
		t.Errorf("Collect sent %d metrics, want 11", count)
	}
}

func TestDBCollector_ExhaustionThreshold_Configurable(t *testing.T) {
	orig := ExhaustionThreshold
	defer func() { ExhaustionThreshold = orig }()

	ResetForTesting()
	r := Init()

	ExhaustionThreshold = 0.5

	c := NewDBCollector(func() DBStats {
		return DBStats{
			MaxOpenConnections: 100,
			InUse:              60, // 60% > 50% custom threshold
		}
	})
	r.MustRegister(c)

	h := Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "lyeve_db_pool_utilization_ratio 0.6") {
		t.Errorf("expected utilization_ratio 0.6, got:\n%s", body)
	}
	if !strings.Contains(body, "lyeve_db_pool_exhaustion_warning 1") {
		t.Errorf("expected exhaustion_warning 1 with lowered threshold, got:\n%s", body)
	}
}
