package memory

import (
	"fmt"
	"strings"
	"testing"
)

// realScenes is the ACTUAL induction input in the store on 2026-07-28: every successful
// mnemonic_scene's capability list and situation projection, verbatim. Hardcoded rather
// than queried so the measurement is reproducible without a database.
//
// It exists because the clustering key was changed on the strength of this data, and a
// key that works on invented fixtures but not on what the kernel really writes would be
// the exact mistake the change was meant to fix.
var realScenes = []struct {
	caps []string
	proj string
}{
	{nil, "goal: Write and verify d3_win.md file | engages: 1 directory, 1 file"},
	{nil, "goal: d3_done.md | engages: 1 file"},
	{[]string{"file_read", "file_read", "file_read"}, "goal: Create and verify notes/alpha.md in runs/rt_smoke/workspace | engages: 1 directory, 1 file"},
	{[]string{"file_read+general_purpose", "file_read"}, "goal: Create and verify beta.md with content 'beta record' | engages: 2 directory, 1 file"},
	{[]string{"file_read", "file_read", "file_read"}, "goal: Create src/two.md, read it, and write summary to out/two.sum.md | engages: 3 directory, 3 file"},
	{[]string{"file_read", "file_read", "file_read"}, "goal: Create and verify notes/alpha.md in runs/rt_diag/workspace | engages: 1 directory, 1 file"},
	{[]string{"general_purpose", "file_read"}, "goal: Create and verify alpha.md in runs/rt_diag4/workspace/notes | engages: 2 directory, 2 file"},
	{[]string{"general_purpose", "file_read", "general_purpose"}, "goal: File processing: create src/one.md, read it, write summary to out/one.sum.md | engages: 2 directory, 2 file"},
	{[]string{"file_read", "file_read"}, "goal: Create and verify notes/beta.md with 'beta record' | engages: 2 directory, 2 file"},
	{[]string{"general_purpose", "general_purpose+file_read", "file_read"}, "goal: Create and verify notes/alpha.md in runs/rt_diag5/workspace | engages: 2 directory, 2 file"},
	{[]string{"general_purpose", "file_read", "general_purpose"}, "goal: Create, read, and summarize src/one.md in runs/rt_diag5/workspace | engages: 5 directory, 4 file"},
	{[]string{"file_read", "file_read", "file_read"}, "goal: Create and verify notes/alpha.md in runs/rt_diag6/workspace | engages: 1 directory, 1 file"},
	{[]string{"file_read", "file_read"}, "goal: runs/rt_fix1/workspace/src/one.md creation and summary | engages: 2 directory, 2 file"},
	{[]string{"file_read+general_purpose"}, "goal: Create pinned.md file | engages: 1 directory, 1 file"},
	{[]string{"file_read"}, "goal: Create pinned.md with user content | engages: 1 directory, 1 file"},
}

func TestInduction_OnRealStoredScenes(t *testing.T) {
	eps := make([]EpisodeShape, 0, len(realScenes))
	for i, s := range realScenes {
		eps = append(eps, EpisodeShape{
			ExperienceID: fmt.Sprintf("exp-%02d", i),
			Trigger:      s.proj,
			Capabilities: s.caps,
			Succeeded:    true,
		})
	}

	got := InduceCandidates(eps, 2)

	total := 0
	for _, c := range got {
		total += c.SampleCount
		t.Logf("n=%d  sig=%q  seq=%v  trigger=%q", c.SampleCount, c.Signature, c.Sequence, c.Trigger)
	}
	t.Logf("episodes=%d  promoted_clusters=%d  episodes_in_clusters=%d", len(eps), len(got), total)

	// The whole point of the change: before it, this corpus produced ZERO clusters at
	// minSamples=2 (19 scenes, 19 distinct keys). If this drops back to zero the tier
	// metric is structurally dead again and no amount of benchmark volume will help.
	// Measured 2026-07-28 on this exact corpus, with each half attributed:
	//   crude trigger + ordered signature (the old key) -> 0 clusters,  0 episodes
	//   hardened trigger + ordered signature            -> 1 cluster,   3 episodes
	//   hardened trigger + capability SET               -> 2 clusters,  5 episodes
	// Both halves are load-bearing, so both numbers are asserted: a regression in
	// either one silently returns the tier metric to "cannot move".
	if len(got) < 2 {
		t.Fatalf("want >=2 promoted clusters on real stored scenes, got %d", len(got))
	}
	if total < 5 {
		t.Fatalf("want >=5 episodes inside clusters, got %d", total)
	}
	// The other half of the risk: loosening the key must not fuse DIFFERENT routines.
	// These two families both engage files with the same coarse capability tags, so a
	// key built only from structure would merge them. Nothing may cluster a
	// verify-what-you-wrote episode with a summarise-into-a-new-file one.
	for _, c := range got {
		verify, summarise := false, false
		for _, id := range c.ExperienceIDs {
			var idx int
			fmt.Sscanf(id, "exp-%d", &idx)
			proj := realScenes[idx].proj
			if strings.Contains(proj, "verify") {
				verify = true
			}
			if strings.Contains(proj, "summar") {
				summarise = true
			}
		}
		if verify && summarise {
			t.Errorf("cluster %q merged two different routines: %v", c.Signature, c.ExperienceIDs)
		}
	}

	// Steps must come from the ordered sequence, not the set.
	for _, c := range got {
		if len(c.Sequence) == 0 {
			t.Errorf("cluster %q carries no representative sequence", c.Signature)
		}
	}
}

func TestCanonicalGoal_StripsVolatileSpecifics(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Create and verify notes/alpha.md in runs/rt_diag6/workspace", "Create and verify in"},
		{"Create and verify beta.md with content 'beta record'", "Create and verify beta.md with content"},
		{"Create pinned.md with user content", "Create pinned.md with user content"},
	}
	for _, c := range cases {
		if got := canonicalGoal(c.in); got != c.want {
			t.Errorf("canonicalGoal(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	// A goal that is nothing BUT a path must not be abstracted out of existence — an
	// empty projection would index the scene on the engages-clause alone.
	if got := canonicalGoal("runs/rt_fix1/workspace/src/one.md"); got == "" {
		t.Error("canonicalGoal must never return empty")
	}
}
