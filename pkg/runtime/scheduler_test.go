package runtime

import "testing"

// GOMAXPROCS is read by the Go runtime itself and wins. CPU_QUOTA comes next.
// Without either the engine leaves the scheduler alone, so Go's default keeps
// following the cgroup's CPU limit while the process runs.
func TestSchedulerProcs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		gomaxprocs string
		cpuQuota   int
		want       int
	}{
		{name: "neither set leaves Go's default", want: 0},
		{name: "CPU_QUOTA alone sets the count", cpuQuota: 3, want: 3},
		{name: "GOMAXPROCS wins over CPU_QUOTA", gomaxprocs: "8", cpuQuota: 3, want: 0},
		{name: "GOMAXPROCS alone is left to the runtime", gomaxprocs: "8", want: 0},
		{name: "CPU_QUOTA of 0 leaves Go's default", cpuQuota: 0, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := schedulerProcs(tt.gomaxprocs, tt.cpuQuota); got != tt.want {
				t.Errorf("schedulerProcs(%q, %d) = %d, want %d", tt.gomaxprocs, tt.cpuQuota, got, tt.want)
			}
		})
	}
}
