package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/cambrian-sh/core/internal/procenv"
)

// TestMain doubles as the fixture process: `<test binary> fixture <mode> …`
// runs a misbehaving child instead of the tests, so every limit is exercised
// against a real process on the real platform.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "fixture" {
		os.Exit(runFixture(os.Args[2], os.Args[3:]))
	}
	goleak.VerifyTestMain(m)
}

func runFixture(mode string, args []string) int {
	switch mode {
	case "echo": // a line-echo "server"
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			fmt.Println(sc.Text())
		}
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		wd, _ := os.Getwd()
		fmt.Println("CWD=" + wd)
	case "stall": // reads requests, never answers
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "bigline":
		fmt.Print(strings.Repeat("x", 256*1024))
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "hog": // allocate and touch memory until something stops us
		var keep [][]byte
		for i := 0; i < 256; i++ {
			b := make([]byte, 16<<20)
			for j := range b {
				b[j] = 1
			}
			keep = append(keep, b)
		}
		fmt.Println("hog survived", len(keep))
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "spin":
		x := 0
		for {
			x++
		}
	case "sleep":
		time.Sleep(60 * time.Second)
	case "forkbomb": // spawn sleepers; report how many started
		started := 0
		var pids []string
		for i := 0; i < 40; i++ {
			c := exec.Command(os.Args[0], "fixture", "sleep")
			if err := c.Start(); err != nil {
				continue
			}
			started++
			pids = append(pids, fmt.Sprint(c.Process.Pid))
		}
		fmt.Printf("started=%d pids=%s\n", started, strings.Join(pids, ","))
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "net": // try to reach the internet
		c, err := net.DialTimeout("tcp", "1.1.1.1:53", 3*time.Second)
		if err == nil {
			_ = c.Close()
		}
		fmt.Printf("net=%v\n", err == nil)
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "write": // try to write inside the workdir and at args[0]
		in := os.WriteFile("inside.txt", []byte("ok"), 0o600)
		out := os.WriteFile(args[0], []byte("escaped"), 0o600)
		fmt.Printf("inside=%v outside=%v\n", in == nil, out == nil)
		_, _ = io.Copy(io.Discard, os.Stdin)
	default:
		return 2
	}
	return 0
}

// testPolicy is the built-in policy rooted in a test directory.
func testPolicy(t *testing.T) *Policy {
	t.Helper()
	p := DefaultPolicy()
	p.WorkRoot = t.TempDir()
	return p
}

func fixture(t *testing.T, mode string, args ...string) Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{Path: exe, Args: append([]string{"fixture", mode}, args...), Env: procenv.ServerEnv(nil, nil)}
}

func waitDone(t *testing.T, p *Process, within time.Duration) bool {
	t.Helper()
	select {
	case <-p.Done():
		return true
	case <-time.After(within):
		return false
	}
}

func TestResolveTier(t *testing.T) {
	p := DefaultPolicy()
	p.Catalog = map[string]CatalogEntry{
		"fetch":   {TrustTier: "official"},
		"slack":   {TrustTier: "community", Server: ServerSpec{Network: NetworkEgress, Hosts: []string{"slack.com"}}},
		"strange": {TrustTier: "unheard-of"},
	}
	cases := []struct {
		name, declared, catalog string
		plugin                  bool
		want                    Tier
		source                  string
	}{
		{"explicit wins over catalog", "first-party", "slack", false, TierFirstParty, "config"},
		{"official catalog entry", "", "fetch", false, TierVetted, "catalog:fetch"},
		{"community catalog entry", "", "slack", false, TierThirdParty, "catalog:slack"},
		{"unknown catalog vocabulary is strict", "", "strange", false, TierThirdParty, "catalog:strange"},
		{"plugin server", "", "", true, TierFirstParty, "plugin"},
		{"unknown server", "", "", false, TierThirdParty, "default"},
		{"catalog id not in catalog", "", "gone", false, TierThirdParty, "default"},
		{"invalid declared tier is strict", "system", "", false, TierThirdParty, "config(invalid:system)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := p.Resolve("x", tc.declared, tc.catalog, tc.plugin, ServerSpec{})
			if s.Tier != tc.want || s.TierSource != tc.source {
				t.Fatalf("got %s (%s), want %s (%s)", s.Tier, s.TierSource, tc.want, tc.source)
			}
		})
	}
	// The catalog's sandbox declaration fills what the server leaves unset.
	s := p.Resolve("x", "", "slack", false, ServerSpec{})
	if s.Server.Network != NetworkEgress || len(s.Server.Hosts) != 1 {
		t.Fatalf("catalog declaration not merged: %+v", s.Server)
	}
	own := p.Resolve("x", "", "slack", false, ServerSpec{Network: NetworkNone})
	if own.Server.Network != NetworkNone {
		t.Fatalf("server's own declaration must win: %+v", own.Server)
	}
}

func TestDefaultTierPolicies(t *testing.T) {
	p := DefaultPolicy()
	if p.DefaultTier != TierThirdParty {
		t.Fatalf("unknown code must default to third-party, got %s", p.DefaultTier)
	}
	tp := p.Tiers[TierThirdParty]
	if tp.OnUnavailable != OnUnavailableRefuse || !tp.ConfineFS || !tp.DenyNetwork || !tp.LowPrivilege {
		t.Fatalf("third-party must fail closed and want fs/network/identity: %+v", tp)
	}
	for _, tier := range []Tier{TierFirstParty, TierVetted} {
		tp := p.Tiers[tier]
		if tp.OnUnavailable != OnUnavailableDegrade || tp.ConfineFS || tp.DenyNetwork {
			t.Fatalf("%s: %+v", tier, tp)
		}
		if tp.MemoryMB == 0 || tp.MaxProcesses == 0 || tp.StallTimeoutSec == 0 || tp.MaxMessageKB == 0 {
			t.Fatalf("%s must have resource limits: %+v", tier, tp)
		}
	}
}

func TestPlanRefusesThirdPartyWhenAControlIsMissing(t *testing.T) {
	p := DefaultPolicy()
	spec := p.Resolve("custom", "", "", false, ServerSpec{})
	tp := spec.tierPolicy(p)
	capab := capability{runner: RunnerNative,
		enforced: map[Control]string{ControlEnv: "", ControlWorkdir: "", ControlKillTree: "job_object",
			ControlOutputCap: "", ControlStallKill: "", ControlMemory: "job_object", ControlProcesses: "job_object",
			ControlCPUTime: "job_object", ControlFSConfine: "low_integrity", ControlLowPrivilege: "low_integrity"},
		missing: map[Control]string{ControlNetwork: "no egress control"},
	}
	rep := plan(spec, tp, capab)
	if !rep.Refused || len(rep.Missing) != 1 || !strings.HasPrefix(rep.Missing[0], "network:") {
		t.Fatalf("third-party without network control must refuse: %+v", rep)
	}
	// Declared egress is a grant: the same platform starts it, with a note.
	spec.Server = ServerSpec{Network: NetworkEgress, Hosts: []string{"api.example.com"}}
	rep = plan(spec, tp, capab)
	if rep.Refused || len(rep.Missing) != 0 || !strings.Contains(strings.Join(rep.Notes, ";"), "api.example.com") {
		t.Fatalf("declared egress must start with a note: %+v", rep)
	}
	// A degrade tier with the same gap starts and reports it.
	v := p.Resolve("fetch", "vetted", "", false, ServerSpec{})
	vtp := v.tierPolicy(p)
	vtp.DenyNetwork = true
	rep = plan(v, vtp, capab)
	if rep.Refused || !rep.Degraded() {
		t.Fatalf("vetted must degrade, not refuse: %+v", rep)
	}
	if !strings.Contains(rep.Summary(), "degraded, missing: network") || !strings.Contains(rep.Summary(), "job_object") {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func TestUnresolvedSpecLaunchesAtTheStrictDefault(t *testing.T) {
	rep := Plan(context.Background(), Spec{ServerID: "forgot"}, fixture(t, "echo"))
	if rep.Tier != TierThirdParty || rep.TierSource != "unresolved" {
		t.Fatalf("an unresolved spec must plan at third-party: %+v", rep)
	}
}

// TestThirdPartyFailsClosedOnThisPlatform starts a third-party server that
// declares no egress with the real platform capability: wherever a wanted
// control is missing (Windows has no native egress control; Linux needs
// bubblewrap and a root kernel), Start must refuse and launch nothing.
func TestThirdPartyFailsClosedOnThisPlatform(t *testing.T) {
	p := testPolicy(t)
	spec := p.Resolve("custom", "", "", false, ServerSpec{})
	planned := Plan(context.Background(), spec, fixture(t, "echo"))
	proc, rep, err := Start(context.Background(), spec, fixture(t, "echo"))
	if len(planned.Missing) == 0 {
		if err != nil {
			t.Fatalf("full containment available, start failed: %v", err)
		}
		_ = proc.Close()
		t.Skip("this platform enforces every third-party control natively")
	}
	var refused *RefusedError
	if !errors.As(err, &refused) || proc != nil || !rep.Refused {
		t.Fatalf("third-party must fail closed, got proc=%v rep=%+v err=%v", proc, rep, err)
	}
	if entries, _ := os.ReadDir(p.WorkRoot); len(entries) != 0 {
		t.Fatalf("a refused launch must not prepare a workdir: %v", entries)
	}
}

func TestDockerRunnerUnavailableRefusesOrFallsBack(t *testing.T) {
	p := testPolicy(t)
	p.DockerPath = filepath.Join(t.TempDir(), "no-such-docker")
	strict := p.Resolve("custom", "", "", false, ServerSpec{Runner: RunnerDocker})
	if _, rep, err := Start(context.Background(), strict, fixture(t, "echo")); err == nil || !rep.Refused {
		t.Fatalf("third-party with an unavailable docker runner must refuse: %+v %v", rep, err)
	}
	lax := p.Resolve("mine", "first-party", "", false, ServerSpec{Runner: RunnerDocker})
	proc, rep, err := Start(context.Background(), lax, fixture(t, "echo"))
	if err != nil {
		t.Fatalf("first-party must fall back to native: %v", err)
	}
	defer func() { _ = proc.Close() }()
	if rep.Runner != RunnerNative || !strings.Contains(strings.Join(rep.Notes, ";"), "fell back to native") {
		t.Fatalf("fallback not reported: %+v", rep)
	}
}

func TestEnvAllowlistWorkdirAndNoSecrets(t *testing.T) {
	t.Setenv("CAMBRIAN_SECRET_KEY", "kernel-secret")
	t.Setenv("OPENAI_API_KEY", "sk-provider")
	p := testPolicy(t)
	spec := p.Resolve("envsrv", "first-party", "", false, ServerSpec{})
	c := fixture(t, "env")
	c.Env = procenv.ServerEnv(nil, map[string]string{"MY_SETTING": "v1"})
	proc, rep, err := Start(context.Background(), spec, c)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(proc)
	_ = proc.Close()
	got := string(out)
	if strings.Contains(got, "kernel-secret") || strings.Contains(got, "sk-provider") {
		t.Fatalf("a kernel secret reached the server:\n%s", got)
	}
	if !strings.Contains(got, "MY_SETTING=v1") {
		t.Fatalf("the server's own setting is missing:\n%s", got)
	}
	wd := WorkdirFor(p, spec)
	if !strings.Contains(strings.ToLower(got), strings.ToLower("CWD="+wd)) {
		t.Fatalf("not started in its workdir %s:\n%s", wd, got)
	}
	if !strings.Contains(strings.ToLower(got), strings.ToLower(filepath.Join(wd, "tmp"))) {
		t.Fatalf("TEMP not redirected into the workdir:\n%s", got)
	}
	if rep.Tier != TierFirstParty {
		t.Fatalf("report: %+v", rep)
	}
}

func TestOutputCapKills(t *testing.T) {
	p := testPolicy(t)
	tp := p.Tiers[TierFirstParty]
	tp.MaxMessageKB = 64
	p.Tiers[TierFirstParty] = tp
	proc, _, err := Start(context.Background(), p.Resolve("big", "first-party", "", false, ServerSpec{}), fixture(t, "bigline"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	_, err = io.ReadAll(proc)
	if !errors.Is(err, ErrOutputCap) {
		t.Fatalf("want ErrOutputCap, got %v", err)
	}
	if !waitDone(t, proc, 10*time.Second) {
		t.Fatal("the process outlived its output cap")
	}
	if !strings.HasPrefix(proc.KillReason(), "output_cap") {
		t.Fatalf("kill reason: %q", proc.KillReason())
	}
}

func TestStallKill(t *testing.T) {
	p := testPolicy(t)
	tp := p.Tiers[TierFirstParty]
	tp.StallTimeoutSec = 1
	p.Tiers[TierFirstParty] = tp
	proc, _, err := Start(context.Background(), p.Resolve("stall", "first-party", "", false, ServerSpec{}), fixture(t, "stall"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	// A notification expects no answer and must not arm the watchdog.
	if _, err := proc.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if waitDone(t, proc, 2500*time.Millisecond) {
		t.Fatal("killed for a notification")
	}
	if _, err := proc.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if !waitDone(t, proc, 10*time.Second) {
		t.Fatal("a stalled server was not killed")
	}
	if !strings.HasPrefix(proc.KillReason(), "stall_kill") {
		t.Fatalf("kill reason: %q", proc.KillReason())
	}
}

func TestEchoRoundTripAndClose(t *testing.T) {
	p := testPolicy(t)
	proc, rep, err := Start(context.Background(), p.Resolve("echo", "vetted", "", false, ServerSpec{}), fixture(t, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Write([]byte(`{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(proc).ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":7`) {
		t.Fatalf("echo: %q %v", line, err)
	}
	if err := proc.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitDone(t, proc, time.Second) {
		t.Fatal("Close returned with the process alive")
	}
	if rep.Tier != TierVetted || rep.Refused {
		t.Fatalf("report: %+v", rep)
	}
	t.Log(rep.Summary())
}

func TestDisabledLaunchesWithEnvOnly(t *testing.T) {
	p := testPolicy(t)
	p.Disabled = true
	proc, rep, err := Start(context.Background(), p.Resolve("x", "", "", false, ServerSpec{}), fixture(t, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Close()
	if !rep.Disabled || !strings.Contains(rep.Summary(), "disabled") {
		t.Fatalf("report: %+v", rep)
	}
}
