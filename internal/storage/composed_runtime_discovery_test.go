package storage

import (
	"slices"
	"testing"
)

// ADR-0145 D12: the composed runtime is registered beside the profile agents only
// when configured; it holds no profile and no capability and is System (never a
// selection candidate).
func TestDiscover_ComposedRuntimeOnlyWhenConfigured(t *testing.T) {
	dir := writeProfileTree(t)
	skills := map[string][]string{"s1": {"mcp:filesystem/read_file"}}

	off, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{SkillTools: skills})
	if err != nil {
		t.Fatal(err)
	}
	for _, da := range off {
		if da.Agent.ID == "composed_worker" {
			t.Fatal("composed runtime registered with the flag off")
		}
	}

	on, err := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{SkillTools: skills, ComposedRuntimeID: "composed_worker"})
	if err != nil {
		t.Fatal(err)
	}
	var rt *DiscoveredAgent
	for i := range on {
		if on[i].Agent.ID == "composed_worker" {
			rt = &on[i]
		}
	}
	if rt == nil {
		t.Fatal("composed runtime not registered with the flag on")
	}
	a := rt.Agent
	if !a.System || a.Profile != "" || a.ExecPath != ProfileRuntimeFile || len(rt.Manifest.Capabilities) != 0 ||
		!slices.Equal(a.LaunchArgs, []string{"--composed", "--agent-id", "composed_worker"}) {
		t.Fatalf("runtime record = %+v / %+v", a, rt.Manifest)
	}
	if !IsProfileRuntime(a) {
		t.Fatal("the runtime record must be pruned like a profile record once the flag is off")
	}
	// An id already taken (here the alpha profile) is refused, never doubled.
	clash, _ := DiscoverFilesystemAgentsWith(dir, nil, DiscoverOptions{SkillTools: skills, ComposedRuntimeID: "alpha"})
	n := 0
	for _, da := range clash {
		if da.Agent.ID == "alpha" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("id clash registered %d agents named alpha", n)
	}
}
