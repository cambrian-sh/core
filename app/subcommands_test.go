package app

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
)

// ENG-1527: the one subcommand table both orchestrator binaries dispatch
// through (app/subcommands.go). Ported from premium's dispatch tests
// (T-20261103-premium-cmd-orchestrator-boots-a-full-kernel-on) when the
// table moved here.

// fakeTable records which subcommand ran and with which args, so routing is
// tested without touching a database or the filesystem.
func fakeTable(ran *string, gotArgs *[]string) []Subcommand {
	var out []Subcommand
	for _, c := range Subcommands(SubcommandSpec{}) {
		name := c.Name
		out = append(out, Subcommand{Name: name, Summary: c.Summary, Run: func(_ context.Context, args []string) int {
			*ran = name
			*gotArgs = args
			return 7
		}})
	}
	return out
}

// An unrecognised first argument is a usage error, never a kernel boot.
func TestDispatchSubcommand_UnknownSubcommandRefusesToBoot(t *testing.T) {
	for _, argv := range [][]string{{"bogus"}, {"confg", "list"}, {"list"}, {"start"}} {
		var ran string
		var got []string
		var stderr bytes.Buffer
		code, handled := DispatchSubcommand(context.Background(), argv, fakeTable(&ran, &got), &stderr)
		if !handled {
			t.Fatalf("%v: dispatch asked to boot the kernel; want a usage error", argv)
		}
		if code != 2 {
			t.Fatalf("%v: exit code = %d, want 2", argv, code)
		}
		if ran != "" {
			t.Fatalf("%v: ran subcommand %q, want none", argv, ran)
		}
		if !strings.Contains(stderr.String(), "unknown subcommand") || !strings.Contains(stderr.String(), "config") {
			t.Fatalf("%v: stderr lacks the error and usage:\n%s", argv, stderr.String())
		}
	}
}

func TestDispatchSubcommand_BootsOnNoArgsOrFlags(t *testing.T) {
	for _, argv := range [][]string{nil, {}, {"-h"}, {"--help"}, {"-somekernelflag=1", "x"}} {
		var ran string
		var got []string
		code, handled := DispatchSubcommand(context.Background(), argv, fakeTable(&ran, &got), &bytes.Buffer{})
		if handled || code != 0 || ran != "" {
			t.Fatalf("%v: handled=%v code=%d ran=%q; want boot (handled=false)", argv, handled, code, ran)
		}
	}
}

func TestDispatchSubcommand_RoutesEveryKnownSubcommandWithItsArgs(t *testing.T) {
	for _, c := range Subcommands(SubcommandSpec{}) {
		var ran string
		var got []string
		code, handled := DispatchSubcommand(context.Background(), []string{c.Name, "a", "b"}, fakeTable(&ran, &got), &bytes.Buffer{})
		if !handled || code != 7 || ran != c.Name {
			t.Fatalf("%s: handled=%v code=%d ran=%q", c.Name, handled, code, ran)
		}
		if strings.Join(got, ",") != "a,b" {
			t.Fatalf("%s: args = %v, want [a b]", c.Name, got)
		}
	}
}

func TestDispatchSubcommand_VersionSpellings(t *testing.T) {
	for _, v := range []string{"version", "--version", "-version"} {
		var ran string
		var got []string
		_, handled := DispatchSubcommand(context.Background(), []string{v}, fakeTable(&ran, &got), &bytes.Buffer{})
		if !handled || ran != "version" {
			t.Fatalf("%s: handled=%v ran=%q, want version", v, handled, ran)
		}
	}
}

func TestDispatchSubcommand_HelpPrintsUsageAndExitsZero(t *testing.T) {
	var ran string
	var got []string
	var stderr bytes.Buffer
	code, handled := DispatchSubcommand(context.Background(), []string{"help"}, fakeTable(&ran, &got), &stderr)
	if !handled || code != 0 || ran != "" || !strings.Contains(stderr.String(), "subcommands:") {
		t.Fatalf("help: handled=%v code=%d ran=%q out=%q", handled, code, ran, stderr.String())
	}
}

// The verb set is pinned: removing or renaming a verb changes the CLI of
// both binaries, which operators and the installer depend on.
func TestSubcommands_VerbSet(t *testing.T) {
	want := []string{"backup", "config", "mcp", "migrate", "principal", "python-env", "setup", "status", "stop", "version"}
	var got []string
	for _, c := range Subcommands(SubcommandSpec{}) {
		if c.Run == nil || c.Summary == "" {
			t.Fatalf("%s has no run func or summary", c.Name)
		}
		got = append(got, c.Name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("subcommands = %v, want %v", got, want)
	}
}
