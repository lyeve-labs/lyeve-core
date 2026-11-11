package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// cgroupV1Unlimited is the floor of what cgroup v1 writes when no limit is
// set: the largest page counter in bytes, which is just under 2^63.
const cgroupV1Unlimited = 1 << 62

// Cgroup is where the process's cgroup limits are read: the cgroup v2 path
// and the cgroup v1 cpu and memory controllers' paths, each "" where the
// process has none. /proc/self/cgroup names them, and a reader that reads the
// limits often parses it once.
//
// The limits sit at the process's own path: the root of the mount in a
// container with its own cgroup namespace, a child of it when the engine runs
// in a limited child cgroup, or a systemd unit's path on a host, where
// MemoryMax and CPUQuota set them. A limit on any parent binds as well, so
// the smallest on the way to the root wins. Cgroup v1 keeps each limit under
// its controller's own mount.
type Cgroup struct {
	// levels is each directory from the process's own up to the root of
	// its mount, with the files read there. The names are joined once here,
	// since the follower reads them every second.
	levels []cgroupLevel
}

// cgroupLevel names the limit files of one directory, each "" where that
// version keeps none.
type cgroupLevel struct {
	cpuMax, memoryMax             string // cgroup v2
	cfsQuota, cfsPeriod, memoryV1 string // cgroup v1
}

// ReadCgroup parses /proc/self/cgroup under fsys, the root of the file
// system. A file it cannot read gives a Cgroup with no paths, whose limits
// read as none.
func ReadCgroup(fsys fs.FS) Cgroup {
	var c Cgroup
	data, err := fs.ReadFile(fsys, "proc/self/cgroup")
	if err != nil {
		return c
	}
	for line := range strings.Lines(string(data)) {
		// hierarchy-id:controllers:path
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 {
			continue
		}
		controllers := strings.Split(fields[1], ",")
		v2 := fields[0] == "0" && fields[1] == ""
		cpuV1 := !v2 && slices.Contains(controllers, "cpu")
		memoryV1 := !v2 && slices.Contains(controllers, "memory")
		if !v2 && !cpuV1 && !memoryV1 {
			continue
		}
		for dir := path.Clean("/" + fields[2]); ; dir = path.Dir(dir) {
			var l cgroupLevel
			if v2 {
				l.cpuMax = path.Join("sys/fs/cgroup", dir, "cpu.max")
				l.memoryMax = path.Join("sys/fs/cgroup", dir, "memory.max")
			}
			if cpuV1 {
				l.cfsQuota = path.Join("sys/fs/cgroup/cpu", dir, "cpu.cfs_quota_us")
				l.cfsPeriod = path.Join("sys/fs/cgroup/cpu", dir, "cpu.cfs_period_us")
			}
			if memoryV1 {
				l.memoryV1 = path.Join("sys/fs/cgroup/memory", dir, "memory.limit_in_bytes")
			}
			c.levels = append(c.levels, l)
			if dir == "/" {
				break
			}
		}
	}
	return c
}

// Limits returns the CPU limit in cores and the memory limit in bytes, each
// 0 for none. Cgroup v2 keeps both files in each directory, so one walk to
// the root reads both.
func (c Cgroup) Limits(fsys fs.FS) (cpu float64, memory int64) {
	var buf [64]byte
	for _, l := range c.levels {
		if l.cpuMax != "" {
			cpu = smallerCPU(cpu, cpuMaxV2(fsys, l.cpuMax, buf[:]))
			memory = smallerLimit(memory, readLimit(fsys, l.memoryMax, buf[:]))
		}
		if l.cfsQuota != "" {
			cpu = smallerCPU(cpu, cpuMaxV1(fsys, l.cfsQuota, l.cfsPeriod, buf[:]))
		}
		if l.memoryV1 != "" {
			memory = smallerLimit(memory, readLimit(fsys, l.memoryV1, buf[:]))
		}
	}
	return cpu, memory
}

// readSmall reads a file of a few bytes into buf, so a reader that reads
// many such files allocates no buffer for each. A file that fills buf is not
// one of them and reads as unreadable.
func readSmall(fsys fs.FS, name string, buf []byte) ([]byte, bool) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	n, err := io.ReadFull(f, buf)
	// ReadFull answers nil only when the file filled buf.
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false
	}
	return bytes.TrimSpace(buf[:n]), true
}

// readLimit reads a memory limit in bytes, or 0 for none or an unreadable
// file. Cgroup v2 writes "max" for none, and cgroup v1 a value just under
// 2^63.
func readLimit(fsys fs.FS, name string, buf []byte) int64 {
	data, ok := readSmall(fsys, name, buf)
	if !ok || string(data) == "max" {
		return 0
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil || n >= cgroupV1Unlimited {
		return 0
	}
	return n
}

// cgroupMemoryLimit returns the memory limit the process's cgroup sets, or 0
// when it sets none or the files cannot be read. fsys is the root of the file
// system.
func cgroupMemoryLimit(fsys fs.FS) int64 {
	_, memory := ReadCgroup(fsys).Limits(fsys)
	return memory
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
func cgroupCPULimit(fsys fs.FS) float64 {
	cpu, _ := ReadCgroup(fsys).Limits(fsys)
	return cpu
}

// cpuMaxV2 reads a cpu.max file. Cgroup v2 writes "<quota> <period>", or
// "max <period>" for no limit.
func cpuMaxV2(fsys fs.FS, name string, buf []byte) float64 {
	data, ok := readSmall(fsys, name, buf)
	if !ok {
		return 0
	}
	quota, period, ok := bytes.Cut(data, []byte(" "))
	if !ok || string(quota) == "max" {
		return 0
	}
	return cpuShare(string(quota), string(bytes.TrimSpace(period)))
}

// cpuMaxV1 reads a cpu.cfs_quota_us file and its cpu.cfs_period_us. Cgroup
// v1 writes -1 in the quota for no limit.
func cpuMaxV1(fsys fs.FS, quotaName, periodName string, buf []byte) float64 {
	quota, ok := readSmall(fsys, quotaName, buf)
	if !ok {
		return 0
	}
	q := string(quota)
	period, ok := readSmall(fsys, periodName, buf)
	if !ok {
		return 0
	}
	return cpuShare(q, string(period))
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
