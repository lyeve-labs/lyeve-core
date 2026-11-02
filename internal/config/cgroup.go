package config

import (
	"fmt"
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

// memoryLimit returns the limit the engine sets Go's soft memory limit from:
// MEMORY_LIMIT_BYTES when it is set above 0, then the limit of the process's
// cgroup, then the machine's total memory, so a VM with no container limit
// follows its own memory. fsys is the root of the file system. GOMEMLIMIT,
// which the runtime reads itself, wins over all three where the soft limit is
// set.
func memoryLimit(override string, fsys fs.FS) (int64, error) {
	if override != "" {
		n, err := strconv.ParseInt(override, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("MEMORY_LIMIT_BYTES must be a number: %w", err)
		}
		if n < 0 {
			return 0, fmt.Errorf("MEMORY_LIMIT_BYTES must be non-negative, got %d", n)
		}
		if n > 0 {
			return n, nil
		}
	}
	if n := cgroupMemoryLimit(fsys); n > 0 {
		return n, nil
	}
	return machineMemory(fsys), nil
}

// machineMemory returns the MemTotal line of /proc/meminfo in bytes, or 0
// when it cannot be read.
func machineMemory(fsys fs.FS) int64 {
	data, err := fs.ReadFile(fsys, "proc/meminfo")
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(data)) {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		// The kernel writes the value in kibibytes: "MemTotal: 65843264 kB".
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || n <= 0 {
			return 0
		}
		return n << 10
	}
	return 0
}

// cgroupCPULimit returns the CPU time the process's cgroup allows, in cores,
// or 0 when it sets no limit or the files cannot be read. fsys is the root of
// the file system. The limit is kept as a fraction, because whether it is one
// core or less decides the scheduler's size.
//
// It is read at the process's own path and every parent, as the memory limit
// is, and the smallest wins. Cgroup v2 writes "<quota> <period>", or "max
// <period>" for no limit, in cpu.max. Cgroup v1 writes the quota and the
// period in two files under the cpu controller's mount, with -1 for no limit.
func cgroupCPULimit(fsys fs.FS) float64 {
	data, err := fs.ReadFile(fsys, "proc/self/cgroup")
	if err != nil {
		return 0
	}
	var limit float64
	for line := range strings.Lines(string(data)) {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}
		switch {
		case fields[0] == "0" && fields[1] == "":
			limit = smallerCPU(limit, cgroupPathCPU(fsys, "sys/fs/cgroup", fields[2], cpuMaxV2))
		case slices.Contains(strings.Split(fields[1], ","), "cpu"):
			limit = smallerCPU(limit, cgroupPathCPU(fsys, "sys/fs/cgroup/cpu", fields[2], cpuMaxV1))
		}
	}
	return limit
}

// cgroupPathCPU returns the smallest CPU limit read by read at dir under
// mount or at any of its parents, or 0 when none sets one.
func cgroupPathCPU(fsys fs.FS, mount, dir string, read func(fs.FS, string) float64) float64 {
	var limit float64
	for dir = path.Clean("/" + dir); ; dir = path.Dir(dir) {
		limit = smallerCPU(limit, read(fsys, path.Join(mount, dir)))
		if dir == "/" {
			return limit
		}
	}
}

// cpuMaxV2 reads cpu.max in dir.
func cpuMaxV2(fsys fs.FS, dir string) float64 {
	data, err := fs.ReadFile(fsys, path.Join(dir, "cpu.max"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	return cpuShare(fields[0], fields[1])
}

// cpuMaxV1 reads cpu.cfs_quota_us and cpu.cfs_period_us in dir.
func cpuMaxV1(fsys fs.FS, dir string) float64 {
	quota, err := fs.ReadFile(fsys, path.Join(dir, "cpu.cfs_quota_us"))
	if err != nil {
		return 0
	}
	period, err := fs.ReadFile(fsys, path.Join(dir, "cpu.cfs_period_us"))
	if err != nil {
		return 0
	}
	return cpuShare(strings.TrimSpace(string(quota)), strings.TrimSpace(string(period)))
}

// cpuShare divides a quota by its period, or returns 0 when either is not a
// positive number, which is how both versions write no limit.
func cpuShare(quota, period string) float64 {
	q, err := strconv.ParseInt(quota, 10, 64)
	if err != nil || q <= 0 {
		return 0
	}
	p, err := strconv.ParseInt(period, 10, 64)
	if err != nil || p <= 0 {
		return 0
	}
	return float64(q) / float64(p)
}

// smallerCPU returns the smaller of two CPU limits, where 0 means none.
func smallerCPU(a, b float64) float64 {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	default:
		return min(a, b)
	}
}

// CgroupCPULimit returns the CPU limit of the process's cgroup in cores, or 0
// for none. fsys is the root of the file system.
func CgroupCPULimit(fsys fs.FS) float64 { return cgroupCPULimit(fsys) }

// CgroupMemoryLimit returns the memory limit of the process's cgroup in
// bytes, or 0 for none. fsys is the root of the file system.
func CgroupMemoryLimit(fsys fs.FS) int64 { return cgroupMemoryLimit(fsys) }

// MachineMemory returns the machine's total memory in bytes, or 0 when
// /proc/meminfo cannot be read. fsys is the root of the file system.
func MachineMemory(fsys fs.FS) int64 { return machineMemory(fsys) }
