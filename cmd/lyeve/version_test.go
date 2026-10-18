package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var versionChildArg = flag.String("version-child", "", "run main with this argument, as the child of TestVersionFlag_PrintsAndExitsWithoutBooting")

// runChild runs the binary's main in a child process with arg as its only
// argument. The child gets no database and no secrets, so a main that boots
// instead of answering fails or hangs, and a hang fails the test.
func runChild(t *testing.T, arg string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestVersionFlag_ChildProcess$", "-test.count=1", "-version-child="+arg)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s did not exit within 30s, so it booted an engine:\n%s", arg, out)
	}
	return string(out), err
}

// TestVersionFlag_PrintsAndExitsWithoutBooting checks that each spelling of
// the version flag prints the version and exits 0 before the engine starts.
func TestVersionFlag_PrintsAndExitsWithoutBooting(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "version"} {
		t.Run(arg, func(t *testing.T) {
			got, err := runChild(t, arg)
			if err != nil {
				t.Fatalf("%s exited with %v:\n%s", arg, err, got)
			}
			want := fmt.Sprintf("lyeve %s (commit %s, built %s)", version, commit, buildDate)
			if !strings.Contains(got, want) {
				t.Errorf("%s printed no %q line:\n%s", arg, want, got)
			}
			if strings.Contains(got, "starting lyeve cms") {
				t.Errorf("%s started the engine:\n%s", arg, got)
			}
		})
	}
}

// TestUnknownArgument_PrintsUsageAndExits2 checks that an argument neither
// the healthcheck nor the version flag answers is refused with the usage
// line and exit status 2 before the engine starts.
func TestUnknownArgument_PrintsUsageAndExits2(t *testing.T) {
	for _, arg := range []string{"--help", "-v", "--verison", "--bogus"} {
		t.Run(arg, func(t *testing.T) {
			got, err := runChild(t, arg)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("%s exited with %v, want exit status 2:\n%s", arg, err, got)
			}
			if want := "usage: lyeve [healthcheck | --version | migrate mark-applied --plugin <name> --version <version>]"; !strings.Contains(got, want) {
				t.Errorf("%s printed no %q line:\n%s", arg, want, got)
			}
			if strings.Contains(got, "starting lyeve cms") {
				t.Errorf("%s started the engine:\n%s", arg, got)
			}
		})
	}
}

// TestMigrateArgument_ReachesTheMigrateCommand checks that the migrate
// command is answered by its own usage line, before the refusal of an unknown
// argument can claim it, and that the engine does not start.
func TestMigrateArgument_ReachesTheMigrateCommand(t *testing.T) {
	got, err := runChild(t, "migrate")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("migrate exited with %v, want exit status 2:\n%s", err, got)
	}
	if want := "usage: lyeve migrate mark-applied --plugin <name> --version <version>"; !strings.Contains(got, want) {
		t.Errorf("migrate printed no %q line:\n%s", want, got)
	}
	if strings.Contains(got, "unknown argument") {
		t.Errorf("migrate was refused as an unknown argument:\n%s", got)
	}
	if strings.Contains(got, "starting lyeve cms") {
		t.Errorf("migrate started the engine:\n%s", got)
	}
}

// TestVersionFlag_ChildProcess is the binary runChild reads. It runs only
// when runChild starts it with an argument.
func TestVersionFlag_ChildProcess(t *testing.T) {
	if *versionChildArg == "" {
		t.Skip("runs only as the child process of runChild")
	}
	os.Args = []string{"lyeve", *versionChildArg}
	main()
}
