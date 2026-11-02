package config

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
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
