package config

import (
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// cgroupV1Unlimited is the floor of what cgroup v1 writes when no limit is
// set: the largest page counter in bytes, which is just under 2^63.
const cgroupV1Unlimited = 1 << 62

// cgroupMemoryLimit returns the memory limit the process's cgroup sets, or 0
// when it sets none or the files cannot be read. fsys is the root of the file
// system.
//
// The limit sits at the process's own path, which /proc/self/cgroup names:
// the root of the mount in a container with its own cgroup namespace, a child
// of it when the engine runs in a limited child cgroup, or a systemd unit's
// path on a host, where MemoryMax sets it. A limit on any parent binds as
// well, so the smallest on the way to the root wins. Cgroup v1 keeps the limit under the memory
// controller's own mount.
func cgroupMemoryLimit(fsys fs.FS) int64 {
	data, err := fs.ReadFile(fsys, "proc/self/cgroup")
	if err != nil {
		return 0
	}
	var limit int64
	for line := range strings.Lines(string(data)) {
		// hierarchy-id:controllers:path
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}
		switch {
		case fields[0] == "0" && fields[1] == "":
			limit = smallerLimit(limit, cgroupPathLimit(fsys, "sys/fs/cgroup", fields[2], "memory.max"))
		case slices.Contains(strings.Split(fields[1], ","), "memory"):
			limit = smallerLimit(limit, cgroupPathLimit(fsys, "sys/fs/cgroup/memory", fields[2], "memory.limit_in_bytes"))
		}
	}
	return limit
}

// cgroupPathLimit returns the smallest limit set in file at dir under mount
// or at any of its parents, or 0 when none sets one.
func cgroupPathLimit(fsys fs.FS, mount, dir, file string) int64 {
	var limit int64
	for dir = path.Clean("/" + dir); ; dir = path.Dir(dir) {
		if data, err := fs.ReadFile(fsys, path.Join(mount, dir, file)); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && n < cgroupV1Unlimited {
				limit = smallerLimit(limit, n)
			}
		}
		if dir == "/" {
			return limit
		}
	}
}

// smallerLimit returns the smaller of two limits, where 0 means none.
func smallerLimit(a, b int64) int64 {
	if a <= 0 {
		return max(b, 0)
	}
	if b <= 0 {
		return a
	}
	return min(a, b)
}
