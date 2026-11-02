package config

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The engine sets its soft memory limit from the cgroup's. In a container the
// limit is in the file at the cgroup mount's root, but under a systemd unit
// or on a cgroup v1 host it is at the process's own path, which only
// /proc/self/cgroup names.
func TestCgroupMemoryLimit(t *testing.T) {
	t.Parallel()

	const mib = 1 << 20
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

	tests := []struct {
		name  string
		files fstest.MapFS
		want  int64
	}{
		{
			name: "a v2 container reads the file at the root",
			files: fstest.MapFS{
				"sys/fs/cgroup/memory.max": file("536870912\n"),
				"proc/self/cgroup":         file("0::/\n"),
			},
			want: 512 * mib,
		},
		{
			name: "an unlimited v2 container reports nothing",
			files: fstest.MapFS{
				"sys/fs/cgroup/memory.max": file("max\n"),
				"proc/self/cgroup":         file("0::/\n"),
			},
			want: 0,
		},
		{
			name: "a limited child under an unlimited container root reads the child",
			files: fstest.MapFS{
				"proc/self/cgroup":                file("0::/engine\n"),
				"sys/fs/cgroup/memory.max":        file("max\n"),
				"sys/fs/cgroup/engine/memory.max": file("268435456\n"),
			},
			want: 256 * mib,
		},
		{
			name: "a systemd unit on a v2 host reads its own path",
			files: fstest.MapFS{
				"proc/self/cgroup":                                    file("0::/system.slice/lyeve.service\n"),
				"sys/fs/cgroup/system.slice/memory.max":               file("max\n"),
				"sys/fs/cgroup/system.slice/lyeve.service/memory.max": file("1073741824\n"),
			},
			want: 1024 * mib,
		},
		{
			name: "a smaller limit on a parent slice wins",
			files: fstest.MapFS{
				"proc/self/cgroup":                                   file("0::/lyeve.slice/lyeve.service\n"),
				"sys/fs/cgroup/lyeve.slice/memory.max":               file("268435456\n"),
				"sys/fs/cgroup/lyeve.slice/lyeve.service/memory.max": file("1073741824\n"),
			},
			want: 256 * mib,
		},
		{
			name: "an unlimited v2 host path reports nothing",
			files: fstest.MapFS{
				"proc/self/cgroup": file("0::/user.slice/user-1000.slice/session-2.scope\n"),
				"sys/fs/cgroup/user.slice/user-1000.slice/session-2.scope/memory.max": file("max\n"),
			},
			want: 0,
		},
		{
			name: "a v1 host reads the memory controller at its own path",
			files: fstest.MapFS{
				"proc/self/cgroup":                           file("12:pids:/system.slice/lyeve.service\n4:memory:/system.slice/lyeve.service\n1:name=systemd:/system.slice/lyeve.service\n"),
				"sys/fs/cgroup/memory/memory.limit_in_bytes": file("9223372036854771712\n"),
				"sys/fs/cgroup/memory/system.slice/lyeve.service/memory.limit_in_bytes": file("805306368\n"),
			},
			want: 768 * mib,
		},
		{
			name: "a v1 container reads the root of its own mount",
			files: fstest.MapFS{
				"proc/self/cgroup":                           file("4:cpuacct,memory:/docker/0123abcd\n"),
				"sys/fs/cgroup/memory/memory.limit_in_bytes": file("2147483648\n"),
			},
			want: 2048 * mib,
		},
		{
			name: "an unlimited v1 host reports nothing",
			files: fstest.MapFS{
				"proc/self/cgroup":                                      file("4:memory:/user.slice\n"),
				"sys/fs/cgroup/memory/memory.limit_in_bytes":            file("9223372036854771712\n"),
				"sys/fs/cgroup/memory/user.slice/memory.limit_in_bytes": file("9223372036854771712\n"),
			},
			want: 0,
		},
		{
			name:  "no cgroup files report nothing",
			files: fstest.MapFS{},
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, cgroupMemoryLimit(tt.files))
		})
	}
}

// MEMORY_LIMIT_BYTES overrides the machine. Without it the cgroup's limit
// applies, and without that the machine's own memory, so a VM with no
// container limit still gets a soft limit.
func TestMemoryLimit_Order(t *testing.T) {
	t.Parallel()

	const mib = 1 << 20
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	limited := fstest.MapFS{
		"proc/self/cgroup":         file("0::/\n"),
		"sys/fs/cgroup/memory.max": file("536870912\n"),
		"proc/meminfo":             file("MemTotal:        2097152 kB\n"),
	}
	unlimited := fstest.MapFS{
		"proc/self/cgroup":         file("0::/\n"),
		"sys/fs/cgroup/memory.max": file("max\n"),
		"proc/meminfo":             file("MemTotal:        2097152 kB\n"),
	}

	tests := []struct {
		name     string
		override string
		files    fstest.MapFS
		want     int64
	}{
		{name: "the variable wins over the cgroup", override: "268435456", files: limited, want: 256 * mib},
		{name: "the variable wins over the machine", override: "268435456", files: unlimited, want: 256 * mib},
		{name: "the cgroup wins over the machine", files: limited, want: 512 * mib},
		{name: "a machine with no cgroup limit uses its memory", files: unlimited, want: 2048 * mib},
		{name: "zero is the same as unset", override: "0", files: limited, want: 512 * mib},
		{name: "nothing readable reports nothing", files: fstest.MapFS{}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := memoryLimit(tt.override, tt.files)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMemoryLimit_RefusesABadOverride(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"1g", "-1"} {
		_, err := memoryLimit(v, fstest.MapFS{})
		assert.Error(t, err, v)
	}
}

// /proc/meminfo gives MemTotal in kibibytes.
func TestMachineMemory(t *testing.T) {
	t.Parallel()

	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	tests := []struct {
		name    string
		meminfo string
		want    int64
	}{
		{
			name:    "a real meminfo",
			meminfo: "MemTotal:       65843264 kB\nMemFree:         1234567 kB\nMemAvailable:   40000000 kB\n",
			want:    65843264 * 1024,
		},
		{name: "MemTotal after other lines", meminfo: "MemFree: 1 kB\nMemTotal: 1024 kB\n", want: 1 << 20},
		{name: "no MemTotal line", meminfo: "MemFree: 1 kB\n", want: 0},
		{name: "a unit other than kB", meminfo: "MemTotal: 1024 MB\n", want: 0},
		{name: "a value that is not a number", meminfo: "MemTotal: lots kB\n", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, machineMemory(fstest.MapFS{"proc/meminfo": file(tt.meminfo)}))
		})
	}
	assert.Equal(t, int64(0), machineMemory(fstest.MapFS{}), "a missing meminfo")
}

// Whether the CPU limit is one core or less decides the scheduler's size, so
// it is read as a fraction, at the process's own path, on either cgroup
// version, and a file that says nothing usable reads as no limit.
func TestCgroupCPULimit(t *testing.T) {
	t.Parallel()

	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

	tests := []struct {
		name  string
		files fstest.MapFS
		want  float64
	}{
		{
			name: "a v2 container reads the file at the root",
			files: fstest.MapFS{
				"proc/self/cgroup":      file("0::/\n"),
				"sys/fs/cgroup/cpu.max": file("150000 100000\n"),
			},
			want: 1.5,
		},
		{
			name: "a half core stays a fraction",
			files: fstest.MapFS{
				"proc/self/cgroup":      file("0::/\n"),
				"sys/fs/cgroup/cpu.max": file("50000 100000\n"),
			},
			want: 0.5,
		},
		{
			name: "max is no limit",
			files: fstest.MapFS{
				"proc/self/cgroup":      file("0::/\n"),
				"sys/fs/cgroup/cpu.max": file("max 100000\n"),
			},
			want: 0,
		},
		{
			name: "a systemd unit on a v2 host reads its own path, and a smaller parent wins",
			files: fstest.MapFS{
				"proc/self/cgroup":                                 file("0::/lyeve.slice/lyeve.service\n"),
				"sys/fs/cgroup/lyeve.slice/cpu.max":                file("100000 100000\n"),
				"sys/fs/cgroup/lyeve.slice/lyeve.service/cpu.max":  file("200000 100000\n"),
				"sys/fs/cgroup/lyeve.slice/lyeve.service/cpu.stat": file("usage_usec 1\n"),
			},
			want: 1,
		},
		{
			name: "a v1 host reads the quota and the period at its own path",
			files: fstest.MapFS{
				"proc/self/cgroup": file("4:memory:/system.slice/lyeve.service\n3:cpu,cpuacct:/system.slice/lyeve.service\n"),
				"sys/fs/cgroup/cpu/system.slice/lyeve.service/cpu.cfs_quota_us":  file("200000\n"),
				"sys/fs/cgroup/cpu/system.slice/lyeve.service/cpu.cfs_period_us": file("100000\n"),
			},
			want: 2,
		},
		{
			name: "a v1 quota of -1 is no limit",
			files: fstest.MapFS{
				"proc/self/cgroup":                    file("3:cpu,cpuacct:/\n"),
				"sys/fs/cgroup/cpu/cpu.cfs_quota_us":  file("-1\n"),
				"sys/fs/cgroup/cpu/cpu.cfs_period_us": file("100000\n"),
			},
			want: 0,
		},
		{
			name: "a malformed cpu.max is no limit",
			files: fstest.MapFS{
				"proc/self/cgroup":      file("0::/\n"),
				"sys/fs/cgroup/cpu.max": file("lots\n"),
			},
			want: 0,
		},
		{
			name: "a zero period is no limit",
			files: fstest.MapFS{
				"proc/self/cgroup":      file("0::/\n"),
				"sys/fs/cgroup/cpu.max": file("100000 0\n"),
			},
			want: 0,
		},
		{
			name:  "no /proc/self/cgroup is no limit",
			files: fstest.MapFS{"sys/fs/cgroup/cpu.max": file("100000 100000\n")},
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.InDelta(t, tt.want, cgroupCPULimit(tt.files), 1e-9)
		})
	}
}
