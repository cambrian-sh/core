//go:build windows

package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cambrian-sh/core/internal/procenv"
)

// ENG-2272 (T-20261120-a-uvx-launched-stdio-mcp-server-fails-inside): a server
// whose workdir an earlier launch had granted to an AppContainer (the ENG-2215
// egress container) was denied that workdir on its next launch on the plain
// low-integrity token: with an AppContainer SID ACE in the DACL the
// low-integrity process could not even open the directory, and uvx died with
// "Failed to initialize cache … os error 183". The low-integrity launch now
// revokes the stale container grant first.

// stalePackageSID is an AppContainer-shaped SID no container on the machine has.
const stalePackageSID = "S-1-15-2-1-2-3-4-5-6-7"

func packageACEs(t *testing.T, path string) []string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for i := uint32(0); dacl != nil && i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if s := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String(); strings.HasPrefix(s, appContainerSIDPrefix) {
			out = append(out, s)
		}
	}
	return out
}

// grantStale gives dir (inheritably) to the stale package SID, as an earlier
// container launch's grantInstanceDir did.
func grantStale(t *testing.T, dir string) {
	t.Helper()
	sid, err := windows.StringToSid(stalePackageSID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cache", "uv"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := grantOne(dir, sid, windows.GENERIC_ALL, true); err != nil {
		t.Fatal(err)
	}
	if got := packageACEs(t, filepath.Join(dir, "cache", "uv")); len(got) == 0 {
		t.Fatal("precondition: the grant did not reach the descendants")
	}
}

func TestRevokeAppContainerACEs_RemovesOnlyPackageGrants(t *testing.T) {
	dir := t.TempDir()
	grantStale(t, dir)
	if err := revokeAppContainerACEs(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, filepath.Join(dir, "cache", "uv")} {
		if got := packageACEs(t, p); len(got) != 0 {
			t.Fatalf("%s still holds package ACEs %v", p, got)
		}
	}
	// The owner keeps its access (the inherited ACEs are re-derived).
	if err := os.WriteFile(filepath.Join(dir, "cache", "uv", "x"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("the owner lost write access: %v", err)
	}
	// Nothing to revoke: a no-op.
	if err := revokeAppContainerACEs(dir); err != nil {
		t.Fatal(err)
	}
}

// The low-integrity launch of a server whose workdir carries a stale container
// grant can use its workdir again.
func TestWindowsLowIntegrityLaunchAfterAContainerGrant(t *testing.T) {
	p := testPolicy(t)
	spec := p.Resolve("conf", "third-party", "", false, ServerSpec{Network: NetworkEgress})
	grantStale(t, WorkdirFor(p, spec))
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	proc, rep, err := Start(context.Background(), spec, fixture(t, "write", outside))
	if err != nil {
		t.Fatalf("start: %v (report %+v)", err, rep)
	}
	defer func() { _ = proc.Close() }()
	line, err := bufio.NewReader(proc).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "inside=true outside=false" {
		t.Fatalf("after a stale container grant the low-integrity process got %q", line)
	}
}

// End to end with the server that failed live: uvx mcp-server-calculator under
// third-party containment (declared egress, no broker ⇒ the low-integrity
// token) in a workdir an earlier container launch had granted, answers MCP
// initialize. Needs uvx on PATH and network access to fetch the package.
func TestWindowsUvxCalculatorStartsAfterAContainerGrant(t *testing.T) {
	uvx, err := exec.LookPath("uvx")
	if err != nil {
		t.Skip("uvx not on PATH")
	}
	if testing.Short() {
		t.Skip("fetches mcp-server-calculator")
	}
	p := testPolicy(t)
	spec := p.Resolve("calculator", "third-party", "", false, ServerSpec{Network: NetworkEgress})
	grantStale(t, WorkdirFor(p, spec))
	proc, rep, err := Start(context.Background(), spec, Command{Path: uvx,
		Args: []string{"--no-progress", "mcp-server-calculator==0.2.1"}, Env: procenv.ServerEnv(nil, nil)})
	if err != nil {
		t.Fatalf("start: %v (report %+v)", err, rep)
	}
	defer func() { _ = proc.Close() }()
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"eng2272","version":"0"}}}` + "\n"
	if _, err := proc.Write([]byte(init)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(proc).ReadString('\n')
		got <- result{line, err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("no initialize response (the server exited?): %v", r.err)
		}
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				ServerInfo struct{ Name string } `json:"serverInfo"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(r.line), &resp); err != nil || resp.ID != 1 || resp.Result.ServerInfo.Name == "" {
			t.Fatalf("initialize response %q (%v)", r.line, err)
		}
		t.Logf("calculator initialized: %s; %s", resp.Result.ServerInfo.Name, rep.Summary())
	case <-time.After(3 * time.Minute):
		t.Fatal("calculator did not answer initialize within 3 minutes")
	}
}
