package runtime

import "testing"

// GOMAXPROCS is read by the Go runtime itself and wins. CPU_QUOTA comes next.
// Without either the engine sets one thread under a CPU limit of one core or
// less on a machine with two or more CPUs, where Go's default would give two.
// Anything else leaves the scheduler alone, so Go's default keeps following
// the cgroup's CPU limit while the process runs.
func TestSchedulerProcs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		gomaxprocs string
		cpuQuota   int
		cpuLimit   float64
		hostCPUs   int
		want       int
	}{
		{name: "neither set leaves Go's default", hostCPUs: 8, want: 0},
		{name: "CPU_QUOTA alone sets the count", cpuQuota: 3, hostCPUs: 8, want: 3},
		{name: "GOMAXPROCS wins over CPU_QUOTA", gomaxprocs: "8", cpuQuota: 3, hostCPUs: 8, want: 0},
		{name: "GOMAXPROCS alone is left to the runtime", gomaxprocs: "8", hostCPUs: 8, want: 0},
		{name: "CPU_QUOTA of 0 leaves Go's default", cpuQuota: 0, hostCPUs: 8, want: 0},
		{name: "a limit of one core on a larger machine gives one", cpuLimit: 1, hostCPUs: 8, want: 1},
		{name: "a limit of half a core on a larger machine gives one", cpuLimit: 0.5, hostCPUs: 2, want: 1},
		{name: "a limit above one core leaves Go's default", cpuLimit: 1.5, hostCPUs: 8, want: 0},
		{name: "a limit of one core on a one-CPU machine leaves Go's default", cpuLimit: 1, hostCPUs: 1, want: 0},
		{name: "CPU_QUOTA wins over a one-core limit", cpuQuota: 2, cpuLimit: 1, hostCPUs: 8, want: 2},
		{name: "GOMAXPROCS wins over a one-core limit", gomaxprocs: "4", cpuLimit: 1, hostCPUs: 8, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := schedulerProcs(tt.gomaxprocs, tt.cpuQuota, tt.cpuLimit, tt.hostCPUs); got != tt.want {
				t.Errorf("schedulerProcs(%q, %d, %v, %d) = %d, want %d", tt.gomaxprocs, tt.cpuQuota, tt.cpuLimit, tt.hostCPUs, got, tt.want)
			}
		})
	}
}
