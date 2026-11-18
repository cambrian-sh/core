//go:build linux

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ENG-2026: a root kernel drops a low_privilege launch to uid 65534; the plan
// must refuse a launch that uid could not boot, naming the path and the fix,
// instead of reporting low_privilege enforced and failing in bwrap.

// lowUIDTree is a directory whose ancestors every uid may read and search, so
// only the modes a case sets decide (the parents t.TempDir makes are 0700).
func lowUIDTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, d := range []string{base, filepath.Dir(base)} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func mkdirMode(t *testing.T, dir string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLowUIDBootBlocker(t *testing.T) {
	base := lowUIDTree(t)
	locked := mkdirMode(t, filepath.Join(base, "locked"), 0o755)
	code := mkdirMode(t, filepath.Join(locked, "agent", "code"), 0o755)
	mkdirMode(t, locked, 0o711)
	open := mkdirMode(t, filepath.Join(base, "open", "code"), 0o700) // its own mode does not matter
	hidden := mkdirMode(t, filepath.Join(base, "hidden"), 0o755)
	inHidden := mkdirMode(t, filepath.Join(hidden, "sub", "code"), 0o755)
	mkdirMode(t, hidden, 0o711) // the hidden dir becomes a tmpfs: binds below it are fine
	hideUnderLocked := mkdirMode(t, filepath.Join(base, "locked2", "state"), 0o755)
	mkdirMode(t, filepath.Dir(hideUnderLocked), 0o711)

	agent := func(binds ...string) Spec {
		a := &AgentSpec{}
		for _, b := range binds {
			a.Binds = append(a.Binds, Bind{Path: b})
		}
		return Spec{Agent: a}
	}
	cases := []struct {
		name  string
		hide  []string
		spec  Spec
		bwrap bool
		want  string // substring of the reason; "" = bootable
	}{
		{"bind under a traverse-only ancestor (bwrap)", nil, agent(code), true, code + " is under " + locked + " (mode 0711)"},
		{"the fix is named", nil, agent(code), true, "chmod 0755 " + locked},
		{"same bind without bwrap only needs search", nil, agent(code), false, ""},
		{"readable ancestors", nil, agent(open), true, ""},
		{"bind inside a hidden directory", []string{hidden}, agent(inHidden), true, ""},
		{"a hidden path under a traverse-only ancestor", []string{hideUnderLocked}, agent(open), true, hideUnderLocked + " is under"},
		{"missing bind is skipped", nil, agent(filepath.Join(locked, "nope")), true, ""},
		{"a bind over the private /tmp is skipped", nil, agent("/tmp"), true, ""},
		{"socket dir", nil, Spec{Agent: &AgentSpec{SocketDir: code}}, true, code + " is under"},
		{"server root", nil, Spec{Server: ServerSpec{Root: code}}, true, code + " is under"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lowUIDBootBlocker(&Policy{HidePaths: tc.hide}, tc.spec, tc.bwrap)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("want bootable, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want a reason containing %q, got %q", tc.want, got)
			}
		})
	}

	mkdirMode(t, locked, 0o701)
	if got := lowUIDBootBlocker(&Policy{}, agent(code), false); got != "" {
		t.Fatalf("o+x alone suffices without bwrap, got %q", got)
	}
	mkdirMode(t, locked, 0o700)
	if got := lowUIDBootBlocker(&Policy{}, agent(code), false); !strings.Contains(got, "chmod 0755 "+locked) {
		t.Fatalf("without bwrap a non-searchable ancestor still blocks, got %q", got)
	}
	mkdirMode(t, locked, 0o755) // so t.TempDir's cleanup can remove the tree
}

type fakeInfo struct {
	os.FileInfo
	mode     os.FileMode
	uid, gid uint32
}

func (f fakeInfo) Mode() os.FileMode { return f.mode }
func (f fakeInfo) Sys() any          { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

func TestLowUIDMayUsesTheMatchingClass(t *testing.T) {
	for _, tc := range []struct {
		mode     os.FileMode
		uid, gid uint32
		need     os.FileMode
		want     bool
	}{
		{0o711, 0, 0, 0o5, false},
		{0o755, 0, 0, 0o5, true},
		{0o711, 0, 0, 0o1, true},
		{0o700, lowUID, 0, 0o5, true},  // owned by 65534: the owner bits
		{0o077, lowUID, 0, 0o5, false}, // the owner bits win even when "other" allows
		{0o750, 0, lowUID, 0o5, true},  // group 65534: the group bits
	} {
		if got := lowUIDMay(fakeInfo{mode: tc.mode, uid: tc.uid, gid: tc.gid}, tc.need); got != tc.want {
			t.Errorf("mode %04o uid %d gid %d need %o: got %v, want %v", tc.mode, tc.uid, tc.gid, tc.need, got, tc.want)
		}
	}
}

// End to end as a root kernel with bubblewrap: StartCmd refuses an agent whose
// code directory sits under a root-owned 0711 directory before anything
// starts, naming the path and the fix, and never reports low_privilege
// enforced, even on a degrade tier; with the ancestor opened to 0755 the same
// agent boots and runs as uid 65534. The CI sandbox job runs it as root;
// elsewhere it skips.
func TestLinuxRootRefusesAnUnbootableBindAtPlanTime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs a root kernel")
	}
	p := DefaultPolicy()
	if bw, why := bwrapUsable(p.BwrapPath); bw == "" {
		t.Skip("bubblewrap not usable here: " + why)
	}
	root, err := os.MkdirTemp("/var/tmp", "cambrian-lowuid-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p.WorkRoot = mkdirMode(t, filepath.Join(root, "work"), 0o711)
	p.HidePaths = append(p.HidePaths, p.WorkRoot)
	for tier, tp := range p.Tiers {
		tp.OnUnavailable = OnUnavailableDegrade // refused anyway: degrading cannot make it boot
		p.Tiers[tier] = tp
	}
	locked := mkdirMode(t, filepath.Join(root, "locked"), 0o755)
	codeDir := mkdirMode(t, filepath.Join(locked, "agent"), 0o755)
	mkdirMode(t, locked, 0o711)
	c := fixture(t, "uid")
	b, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	c.Path = filepath.Join(codeDir, filepath.Base(c.Path))
	if err := os.WriteFile(c.Path, b, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := p.Resolve("custom", "", "", false, ServerSpec{})
	if !spec.tierPolicy(p).LowPrivilege {
		t.Fatalf("tier %s does not drop the uid; the test needs one that does", spec.Tier)
	}
	spec.ServerID = "lowuid-probe"
	spec.Agent = &AgentSpec{Binds: []Bind{{Path: codeDir}}, Ephemeral: true}
	_, rep, err := StartCmd(context.Background(), spec, c, nil)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want *RefusedError, got %v (%s)", err, rep.Summary())
	}
	for _, s := range []string{codeDir, locked + " (mode 0711)", "chmod 0755 " + locked} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("the refusal must name %q: %v", s, err)
		}
	}
	if slices.Contains(rep.Enforced, ControlLowPrivilege) || !rep.Refused {
		t.Fatalf("an unbootable plan must not report low_privilege enforced: %s", rep.Summary())
	}
	if pr := Plan(context.Background(), spec, c); !pr.Refused || slices.Contains(pr.Enforced, ControlLowPrivilege) {
		t.Fatalf("Plan must refuse too: %s", pr.Summary())
	}
	if m, _ := filepath.Glob(filepath.Join(p.WorkRoot, "*", "*")); len(m) != 0 {
		t.Fatalf("a refused launch created a workdir: %v", m)
	}
	t.Logf("refused: %v", err)

	mkdirMode(t, locked, 0o755)
	var stdout io.ReadCloser
	l, rep, err := StartCmd(context.Background(), spec, c, func(cmd *exec.Cmd) error {
		var perr error
		stdout, perr = cmd.StdoutPipe()
		return perr
	})
	if err != nil {
		t.Fatalf("with 0755 ancestors the agent must boot: %v (%s)", err, rep.Summary())
	}
	out, _ := io.ReadAll(stdout)
	done := make(chan struct{})
	go func() { _ = l.Cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = l.Kill()
		<-done
		t.Fatal("agent did not exit")
	}
	l.Release()
	if !slices.Contains(rep.Enforced, ControlLowPrivilege) {
		t.Fatalf("low_privilege must be enforced once bootable: %s", rep.Summary())
	}
	if strings.TrimSpace(string(out)) != "uid=65534" {
		t.Fatalf("want the agent to run as uid 65534, got %q", out)
	}
	t.Log(rep.Summary())
}
