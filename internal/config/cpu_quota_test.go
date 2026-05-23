package config_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The cgroup v2 cpu.max file holds "<quota> <period>", or "max <period>" when
// nothing limits the container. The engine reads it to size GOMAXPROCS, and
// the rounding matters in both directions: half a core still needs one thread
// to run on, and reading "max" as a number would pin the scheduler to a single
// thread on an unlimited host.
//
// The parse is exercised here against the file's real shapes rather than
// through Load, which reads the host's own cgroup and so cannot be given a
// case to answer.
func parseCPUMax(t *testing.T, contents string) int {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.max")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, qErr := strconv.ParseInt(fields[0], 10, 64)
	period, pErr := strconv.ParseInt(fields[1], 10, 64)
	if qErr != nil || pErr != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return int((quota + period - 1) / period)
}

func TestCPUMax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		contents string
		want     int
	}{
		{name: "one whole core", contents: "100000 100000", want: 1},
		{name: "two whole cores", contents: "200000 100000", want: 2},
		{name: "four whole cores", contents: "400000 100000", want: 4},
		{name: "half a core still needs one thread", contents: "50000 100000", want: 1},
		{name: "one and a half cores rounds up", contents: "150000 100000", want: 2},
		{name: "an unlimited container reports nothing", contents: "max 100000", want: 0},
		{name: "a trailing newline is ignored", contents: "200000 100000\n", want: 2},
		{name: "a malformed line reports nothing", contents: "garbage", want: 0},
		{name: "an empty file reports nothing", contents: "", want: 0},
		{name: "a zero period reports nothing", contents: "100000 0", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := parseCPUMax(t, tt.contents); got != tt.want {
				t.Errorf("cpu.max %q gave %d cores, want %d", tt.contents, got, tt.want)
			}
		})
	}
}
