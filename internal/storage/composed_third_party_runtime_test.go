package storage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ENG-2083: the third-party composed runtime is registered beside the shared one
// only when a valid third-party profile exists; it is System (never selected),
// at trust tier third-party, and launched as its own agent id.
func TestDiscover_ThirdPartyComposedRuntimeOnlyWithAThirdPartyProfile(t *testing.T) {
	dir := writeProfileTree(t)
	skills := map[string][]string{"s1": {"mcp:filesystem/read_file"}}
	opt := DiscoverOptions{SkillTools: skills, ComposedRuntimeID: "composed_worker",
		ComposedThirdPartyRuntimeID: "composed_worker_third_party"}
	find := func(got []DiscoveredAgent, id string) *DiscoveredAgent {
		for i := range got {
			if got[i].Agent.ID == id {
				return &got[i]
			}
		}
		return nil
	}

	first, err := DiscoverFilesystemAgentsWith(dir, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	if find(first, "composed_worker") == nil {
		t.Fatal("the shared runtime must be registered")
	}
	if find(first, "composed_worker_third_party") != nil {
		t.Fatal("a third-party runtime registered without any third-party profile")
	}

	tp := strings.ReplaceAll(strings.ReplaceAll(testProfile, "%s", "outsider"), "trust_tier: first-party", "trust_tier: third-party")
	if err := os.WriteFile(filepath.Join(dir, ProfilesDirName, "outsider.yaml"), []byte(tp), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverFilesystemAgentsWith(dir, nil, opt)
	if err != nil {
		t.Fatal(err)
	}
	shared, third := find(got, "composed_worker"), find(got, "composed_worker_third_party")
	if shared == nil || third == nil {
		t.Fatalf("both runtimes must be registered: shared=%v third=%v", shared != nil, third != nil)
	}
	if shared.Agent.TrustTier != "first-party" {
		t.Fatalf("shared runtime tier = %q", shared.Agent.TrustTier)
	}
	a := third.Agent
	if !a.System || a.TrustTier != "third-party" || a.Profile != "" || a.ExecPath != ProfileRuntimeFile ||
		!slices.Equal(a.LaunchArgs, []string{"--composed", "--agent-id", "composed_worker_third_party"}) {
		t.Fatalf("third-party runtime record = %+v", a)
	}
	if !IsProfileRuntime(a) {
		t.Fatal("the third-party runtime must be pruned like a profile record once it is no longer discovered")
	}
	// Without the shared runtime configured, neither is registered.
	off, _ := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{SkillTools: skills, ComposedThirdPartyRuntimeID: "composed_worker_third_party"})
	if find(off, "composed_worker_third_party") != nil {
		t.Fatal("the third-party runtime registered with composed workers off")
	}
}
