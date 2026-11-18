//go:build windows

package sandbox

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// ADR-0157 S5 (D2.4): the Windows AppContainer-class agent runner, against the
// real OS — lowbox tokens, ACL grants and AccessCheck, and real processes.

// acFixture is a kernel-shaped tree: a hidden configs/ with config.db and
// secret.key, the agent's code (a kernel-known read path), a declared writable
// cache and an unrelated user file.
type acFixture struct {
	root, configs, configDB, secretKey, code, codeFile, cache, public string
}

func newACFixture(t *testing.T) acFixture {
	t.Helper()
	root := shortTempDir(t)
	f := acFixture{root: root,
		configs: filepath.Join(root, "kernel", "configs"), code: filepath.Join(root, "kernel", "agents"),
		cache: filepath.Join(root, "caches", "hf"), public: filepath.Join(root, "public", "readme.txt")}
	f.configDB = filepath.Join(f.configs, "config.db")
	f.secretKey = filepath.Join(f.configs, "secret.key")
	f.codeFile = filepath.Join(f.code, "agent.py")
	for _, p := range []string{f.configDB, f.secretKey, f.codeFile, f.public} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(f.cache, 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f acFixture) spec(t *testing.T, tier Tier, identity string, extra ...Bind) (*Policy, Spec) {
	t.Helper()
	p := agentPolicy(t, filepath.Join(f.root, "kernel"), f.configs)
	binds := append([]Bind{{Path: f.code}, {Path: f.cache, Write: true, Private: true}}, extra...)
	s := agentSpec(p, tier, "", binds...)
	s.ServerID = identity + "-0000aaaa"
	s.Agent.Identity = identity
	return p, s
}

func mustCanRead(t *testing.T, tok windows.Token, path string) bool {
	t.Helper()
	ok, err := canRead(tok, path)
	if err != nil {
		t.Fatalf("canRead %s: %v", path, err)
	}
	return ok
}

func TestContainerNameIsBoundedAndStable(t *testing.T) {
	short := containerName("cambrian.agent.", "chat_agent-1a2b3c4d")
	if short != "cambrian.agent.chat_agent-1a2b3c4d" {
		t.Fatalf("short name = %q", short)
	}
	long := strings.Repeat("telegram_ingress_support_", 4)
	a, b := containerName("cambrian.agent.", long), containerName("cambrian.agent.", long+"x")
	if len(a) > 64 || len(b) > 64 || a == b || a != containerName("cambrian.agent.", long) {
		t.Fatalf("long names: %q %q", a, b)
	}
}

// The kernel's state is unreadable from the container, the granted paths are
// readable, and a declared path is granted to that agent only.
func TestAppContainerHidesStateAndGrantsBinds(t *testing.T) {
	f := newACFixture(t)
	p, spec := f.spec(t, TierFirstParty, "probe")
	tp := spec.tierPolicy(p)
	ac, pl := appContainerFor(p, spec, tp, true)
	if !pl.ok {
		t.Fatalf("appcontainer unusable: %s", pl.why)
	}
	defer ac.close()
	if len(pl.readable) != 0 {
		t.Fatalf("hidden paths readable from the container: %v", pl.readable)
	}
	for _, hidden := range []string{f.configDB, f.secretKey, f.configs, f.public} {
		if mustCanRead(t, ac.token, hidden) {
			t.Errorf("%s is readable from the container", hidden)
		}
	}
	if !mustCanRead(t, ac.token, f.codeFile) {
		t.Errorf("the agent's code %s is not readable", f.codeFile)
	}
	if !mustCanRead(t, ac.token, f.cache) {
		t.Errorf("the declared cache %s is not readable", f.cache)
	}
	// Another agent shares the runtime grant but not the declared cache.
	_, other := f.spec(t, TierFirstParty, "other")
	other.Agent.Binds = []Bind{{Path: f.code}}
	ac2, pl2 := appContainerFor(p, other, tp, true)
	if !pl2.ok {
		t.Fatalf("second container: %s", pl2.why)
	}
	defer ac2.close()
	if !mustCanRead(t, ac2.token, f.codeFile) || mustCanRead(t, ac2.token, f.cache) {
		t.Fatal("the runtime grant must be shared and the declared path private to its agent")
	}
	// The grants are ACEs for the capability SIDs, never the package SID of an
	// instance (an instance's scratch is the only package-SID grant).
	if _, has, err := daclState(f.code, ac.runtime, accessRead); err != nil || !has {
		t.Fatalf("runtime capability ACE on %s: has=%v err=%v", f.code, has, err)
	}
	if _, has, err := daclState(f.cache, ac.agent, accessModify); err != nil || !has {
		t.Fatalf("agent capability ACE on %s: has=%v err=%v", f.cache, has, err)
	}
}

// uv/pip hardlinks keep a protected DACL that an inherited ACE never reaches:
// the grant adds an explicit ACE there.
func TestAppContainerGrantReachesProtectedDescendants(t *testing.T) {
	f := newACFixture(t)
	protected := filepath.Join(f.code, "site-packages", "grpc", "__init__.py")
	if err := os.MkdirAll(filepath.Dir(protected), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	setProtectedOwnerOnly(t, protected)
	p, spec := f.spec(t, TierFirstParty, "protected")
	ac, pl := appContainerFor(p, spec, spec.tierPolicy(p), true)
	if !pl.ok {
		t.Fatalf("appcontainer unusable: %s", pl.why)
	}
	defer ac.close()
	if !mustCanRead(t, ac.token, protected) {
		t.Fatal("a protected descendant of a granted path is not readable")
	}
}

// A bind that contains a hidden path is never granted (it would expose it).
func TestAppContainerSkipsABindThatContainsKernelState(t *testing.T) {
	f := newACFixture(t)
	p, spec := f.spec(t, TierFirstParty, "wide", Bind{Path: f.root, Private: true})
	ac, pl := appContainerFor(p, spec, spec.tierPolicy(p), true)
	if !pl.ok {
		t.Fatalf("appcontainer unusable: %s", pl.why)
	}
	defer ac.close()
	if !slices.ContainsFunc(pl.notes, func(n string) bool { return strings.Contains(n, "contains kernel state") }) {
		t.Fatalf("notes do not name the skipped bind: %v", pl.notes)
	}
	if mustCanRead(t, ac.token, f.configDB) || mustCanRead(t, ac.token, f.public) {
		t.Fatal("a bind containing kernel state was granted")
	}
}

// A path the kernel cannot grant (no WRITE_DAC: an Administrators-owned
// interpreter) makes the AppContainer unusable: the plan falls back to low
// integrity, names hide_state missing and gives the administrator command.
func TestAppContainerFallsBackWhenAGrantIsImpossible(t *testing.T) {
	f := newACFixture(t)
	locked := filepath.Join(f.root, "locked-python")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	denyOwnerWriteDAC(t, locked)
	p, spec := f.spec(t, TierFirstParty, "locked", Bind{Path: locked})
	rep := Plan(context.Background(), spec, Command{Path: "x"})
	if slices.Contains(rep.Mechanisms, MechanismAppContainer) || slices.Contains(rep.Enforced, ControlHideState) {
		t.Fatalf("an ungrantable bind still planned the AppContainer: %s", rep.Summary())
	}
	if !slices.ContainsFunc(rep.Missing, func(m string) bool {
		return strings.HasPrefix(m, "hide_state:") && strings.Contains(m, "unusable") && strings.Contains(m, "icacls")
	}) {
		t.Fatalf("the fallback is not named with its fix: %s", rep.Summary())
	}
	if !slices.Contains(rep.Mechanisms, "low_integrity") || rep.Refused {
		t.Fatalf("first-party must degrade to low integrity: %s", rep.Summary())
	}
	_ = p
}

// A real process in the container: it cannot read the kernel's state, reads
// its code, writes its scratch, and — on a kernel-only tier — has no network.
func TestAppContainerRealProcess(t *testing.T) {
	f := newACFixture(t)
	p, spec := f.spec(t, TierThirdParty, "vendor")
	tiers := map[Tier]TierPolicy{}
	for k, v := range p.Tiers {
		tiers[k] = v
	}
	tp := tiers[TierThirdParty]
	tp.DenyNetwork = true
	tiers[TierThirdParty] = tp
	p.Tiers = tiers
	sockDir := filepath.Join(p.WorkRoot, "sock", "0000aaaa")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	spec.Agent.SocketDir = sockDir
	run := func(c Command) (string, Report) {
		t.Helper()
		spec := spec
		a := *spec.Agent
		a.Binds = append([]Bind{{Path: filepath.Dir(c.Path)}}, a.Binds...)
		spec.Agent = &a
		var stdout io.ReadCloser
		l, rep, err := StartCmd(context.Background(), spec, c, func(cmd *exec.Cmd) error {
			var perr error
			stdout, perr = cmd.StdoutPipe()
			return perr
		})
		if err != nil {
			t.Fatalf("start: %v (%s)", err, rep.Summary())
		}
		out, _ := io.ReadAll(stdout)
		_ = l.Cmd.Wait()
		l.Release()
		return string(out), rep
	}
	out, rep := run(fixture(t, "read", f.configDB, f.secretKey, f.codeFile, f.public))
	if !slices.Contains(rep.Mechanisms, MechanismAppContainer) || rep.Degraded() {
		t.Fatalf("third-party agent not fully contained: %s", rep.Summary())
	}
	for path, want := range map[string]string{f.configDB: "denied", f.secretKey: "denied", f.codeFile: "ok", f.public: "denied"} {
		if !strings.Contains(out, path+"="+want) {
			t.Errorf("%s: want %s in\n%s", path, want, out)
		}
	}
	c := fixture(t, "write", filepath.Join(f.configs, "escaped.txt"))
	c.Args = append(c.Args[:0:0], c.Args...)
	out, _ = run(c)
	if !strings.Contains(out, "inside=true outside=false") {
		t.Errorf("write confinement: %s", out)
	}
	net := fixture(t, "net")
	out, _ = run(net)
	if !strings.Contains(out, "net=false") {
		t.Errorf("a kernel-only AppContainer reached the network: %s", out)
	}
}

// setProtectedOwnerOnly gives path a protected DACL granting only the current
// user, the shape uv's hardlinked files have.
func setProtectedOwnerOnly(t *testing.T, path string) {
	t.Helper()
	user := currentUserSID(t)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{AccessPermissions: windows.GENERIC_ALL,
		AccessMode: windows.SET_ACCESS, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

// denyOwnerWriteDAC leaves path readable but takes WRITE_DAC away from its
// owner (an OWNER RIGHTS ACE replaces the owner's implicit rights).
func denyOwnerWriteDAC(t *testing.T, path string) {
	t.Helper()
	owner, err := windows.StringToSid("S-1-3-4")
	if err != nil {
		t.Fatal(err)
	}
	user := currentUserSID(t)
	entry := func(sid *windows.SID, tt windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_READ | windows.GENERIC_EXECUTE,
			AccessMode: windows.SET_ACCESS, Inheritance: windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: tt, TrusteeValue: windows.TrusteeValueFromSID(sid)}}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry(owner, windows.TRUSTEE_IS_WELL_KNOWN_GROUP), entry(user, windows.TRUSTEE_IS_USER)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func currentUserSID(t *testing.T) *windows.SID {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return tu.User.Sid
}

// ADR-0157 D7.7.5 (P2): the kernel-managed Python generation is pre-granted
// while empty (GrantAgentRuntimeRead), so what the build then writes inherits
// the runtime ACE and the agent reads it; a Granted bind is NOT walked — the
// proof is a protected descendant the walk would have granted staying
// unreadable.
func TestAppContainerPreGrantedTreeIsInheritedAndNotWalked(t *testing.T) {
	f := newACFixture(t)
	gen := filepath.Join(f.root, "runtime", "python", "base", "0123456789abcdef")
	if err := os.MkdirAll(gen, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := GrantAgentRuntimeRead(gen); err != nil {
		t.Fatalf("pre-grant: %v", err)
	}
	inherited := filepath.Join(gen, "Lib", "site-packages", "cambrian_agent_sdk", "__init__.py")
	protected := filepath.Join(gen, "Lib", "site-packages", "hardlinked", "__init__.py")
	for _, p := range []string{inherited, protected} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setProtectedOwnerOnly(t, protected)

	p, spec := f.spec(t, TierFirstParty, "pregranted", Bind{Path: gen, Granted: true})
	ac, pl := appContainerFor(p, spec, spec.tierPolicy(p), true)
	if !pl.ok {
		t.Fatalf("appcontainer unusable: %s", pl.why)
	}
	defer ac.close()
	if !mustCanRead(t, ac.token, inherited) {
		t.Fatal("a file written after the pre-grant did not inherit the runtime ACE")
	}
	if mustCanRead(t, ac.token, protected) {
		t.Fatal("a Granted bind was walked: its protected descendant was granted")
	}
	if !mustCanRead(t, ac.token, f.codeFile) {
		t.Fatal("ordinary binds must still be granted")
	}
	if mustCanRead(t, ac.token, f.configDB) {
		t.Fatal("kernel state readable")
	}
}
