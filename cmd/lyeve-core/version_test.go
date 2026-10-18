package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var versionChildArg = flag.String("version-child", "", "run main with this argument, as the child of TestVersionFlag_PrintsAndExitsWithoutBooting")

// TestVersionFlag_PrintsAndExitsWithoutBooting runs the binary's main in a
// child process with each spelling of the version flag. The child gets no
// database and no secrets, so a main that boots instead of answering fails
// or hangs rather than printing the version and exiting 0.
func TestVersionFlag_PrintsAndExitsWithoutBooting(t *testing.T) {
	for _, arg := range []string{"--version", "-version", "version"} {
		t.Run(arg, func(t *testing.T) {
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
			if err != nil {
				t.Fatalf("%s exited with %v:\n%s", arg, err, out)
			}
			got := string(out)
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

// TestVersionFlag_ChildProcess is the binary TestVersionFlag_PrintsAndExitsWithoutBooting
// reads. It runs only when that test starts it with an argument.
func TestVersionFlag_ChildProcess(t *testing.T) {
	if *versionChildArg == "" {
		t.Skip("runs only as the child process of TestVersionFlag_PrintsAndExitsWithoutBooting")
	}
	os.Args = []string{"lyeve", *versionChildArg}
	main()
}
