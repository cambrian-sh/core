package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ENG-1527, end to end through the built binary: a typo'd subcommand is a
// usage error (exit 2) that never boots the kernel, and flag-shaped arguments
// still reach the kernel's flag parsing.

func buildOrchestrator(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the orchestrator binary")
	}
	bin := filepath.Join(t.TempDir(), "orchestrator")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runIn runs the binary in an empty working directory with a database that
// cannot be reached, so a kernel boot would be visible: log lines, files in
// the working directory, or no exit within the deadline.
func runIn(t *testing.T, bin string, args ...string) (code int, stdout, stderr, dir string) {
	t.Helper()
	dir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DATABASE_URL=postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%v: still running after 60s (the kernel booted?)\nstderr:\n%s", args, se.String())
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("%v: %v", args, err)
	}
	return code, so.String(), se.String(), dir
}

func TestBinary_TypoSubcommandIsAUsageErrorAndNeverBoots(t *testing.T) {
	bin := buildOrchestrator(t)
	for _, argv := range [][]string{{"confg", "list"}, {"start"}} {
		code, stdout, stderr, dir := runIn(t, bin, argv...)
		if code != 2 {
			t.Fatalf("%v: exit %d, want 2\nstderr:\n%s", argv, code, stderr)
		}
		if !strings.Contains(stderr, "unknown subcommand") || !strings.Contains(stderr, "the kernel was NOT started") ||
			!strings.Contains(stderr, "migrate") {
			t.Fatalf("%v: stderr lacks the usage error:\n%s", argv, stderr)
		}
		// Nothing but the usage text: no kernel log line, no stdout.
		if stdout != "" || strings.Contains(stderr, "level=") || strings.Contains(stderr, "Kernel") {
			t.Fatalf("%v: the kernel started:\nstdout:\n%s\nstderr:\n%s", argv, stdout, stderr)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Fatalf("%v: the binary wrote %d entries into its working directory", argv, len(ents))
		}
	}
}

func TestBinary_HelpFlagsReachFlagParsing(t *testing.T) {
	bin := buildOrchestrator(t)
	for _, f := range []string{"-h", "--help"} {
		code, _, stderr, _ := runIn(t, bin, f)
		// flag.Parse (app.Run's first statement) prints the kernel's usage and
		// exits 0 on -h/--help — not the subcommand usage error.
		if code != 0 || !strings.Contains(stderr, "Usage of") || strings.Contains(stderr, "unknown subcommand") {
			t.Fatalf("%s: exit %d, want flag usage with 0\nstderr:\n%s", f, code, stderr)
		}
	}
}

func TestBinary_KnownSubcommandStillRuns(t *testing.T) {
	bin := buildOrchestrator(t)
	code, stdout, stderr, _ := runIn(t, bin, "version")
	if code != 0 || strings.TrimSpace(stdout) != "cambrian-core dev" {
		t.Fatalf("version: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
