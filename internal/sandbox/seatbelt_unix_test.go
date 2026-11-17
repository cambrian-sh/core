//go:build !windows

package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// The Seatbelt input is resolved on the host (Seatbelt matches resolved
// paths: /var is /private/var on macOS), hidden files are told from hidden
// directories, and the network follows the tier and the declared egress.
func TestSeatbeltInputForResolvesPaths(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	conf := filepath.Join(real, "configs")
	key := filepath.Join(conf, "secret.key")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	p := agentPolicy(t, filepath.Join(link, "configs"), filepath.Join(link, "configs", "secret.key"))
	spec := agentSpec(p, TierThirdParty, filepath.Join(link, "sock"), Bind{Path: filepath.Join(link, "agents")})
	tp := spec.tierPolicy(p)
	tp.DenyNetwork = true
	in := seatbeltInputFor(p, spec, tp, link)
	realConf, _ := filepath.EvalSymlinks(conf)
	realKey, _ := filepath.EvalSymlinks(key)
	realRoot, _ := filepath.EvalSymlinks(real)
	if in.Workdir != realRoot || !slices.Contains(in.Hide, realConf) || !in.HideFiles[realKey] || in.HideFiles[realConf] {
		t.Fatalf("input not resolved: %+v", in)
	}
	if !in.DenyNetwork {
		t.Fatal("a kernel-only tier must deny the network")
	}
	spec.Server.Network = NetworkEgress
	if seatbeltInputFor(p, spec, tp, link).DenyNetwork {
		t.Fatal("declared egress must keep the network")
	}
	if runtime.GOOS != "darwin" && useSeatbelt(spec, tp) {
		t.Fatal("Seatbelt is macOS-only")
	}
}
