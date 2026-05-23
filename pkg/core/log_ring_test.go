package core

import "testing"

type fixedRing struct{ stats LogRingStats }

func (r fixedRing) Subscribe(int) (uint64, <-chan LogRecord)       { return 1, nil }
func (r fixedRing) Unsubscribe(uint64)                             {}
func (r fixedRing) Snapshot(func(LogRecord) bool, int) []LogRecord { return nil }
func (r fixedRing) Stats() LogRingStats                            { return r.stats }

type ringHost struct {
	*stubHost
	ring LogRing
}

func (h *ringHost) LogRing() LogRing { return h.ring }

// The plugin reads the inner host's ring through the scoped host, with no
// capability in between: the records reach its sink through the slog chain
// already.
func TestScopedHost_LogRing_Forwards(t *testing.T) {
	ring := fixedRing{stats: LogRingStats{Capacity: 10000, Entries: 3, Sequence: 3}}
	sh := NewScopedHost(&ringHost{stubHost: &stubHost{}, ring: ring}, "logging", 0)

	var host Host = sh
	p, ok := host.(LogRingProvider)
	if !ok {
		t.Fatal("ScopedHost must satisfy LogRingProvider through the Host interface")
	}
	got := p.LogRing()
	if got == nil {
		t.Fatal("LogRing() = nil, want the inner host's ring")
	}
	if got.Stats() != ring.stats {
		t.Errorf("Stats() = %+v, want %+v", got.Stats(), ring.stats)
	}
}

// A host built without a ring answers nil rather than a ring that holds
// nothing, so the plugin can say the ring is unavailable.
func TestScopedHost_LogRing_NilWithoutProvider(t *testing.T) {
	sh := NewScopedHost(&stubHost{}, "logging", 0)
	if got := sh.LogRing(); got != nil {
		t.Errorf("LogRing() = %v, want nil from a host with no ring", got)
	}
}

func TestParseLogLevel_RoundTrip(t *testing.T) {
	for _, l := range []LogLevel{LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError} {
		if got := ParseLogLevel(l.String()); got != l {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", l.String(), got, l)
		}
	}
	if got := ParseLogLevel("verbose"); got != LogLevelInfo {
		t.Errorf("ParseLogLevel(unknown) = %v, want Info", got)
	}
	if got := ParseLogLevel("warning"); got != LogLevelWarn {
		t.Errorf("ParseLogLevel(warning) = %v, want Warn", got)
	}
}
