package domain

import "sort"

// The contribution lane's capability vocabulary (ADR-0127 D9, slice CL-3).
//
// A worker's manifest arrives in whatever spelling its local MCP servers use:
// `read_file`, `read-file`, `Read File`. Kernel agents declare capabilities in
// whatever spelling their manifests use, canonicalized by ADR-0067's
// NormalizeCapability. Two vocabularies for the same idea is how routing
// silently misses a machine that can serve a step — so contributed manifests
// enter THE SAME vocabulary at the moment they enter the kernel (the hub's
// poll, InMemoryFleet.RegisterWorker), tagged with the owner principal the
// fleet already knows.
//
// The normalization is CARRIED ALONGSIDE, never substituted:
//
//   - the wire name is untouched. The broker calls its local servers by their
//     real names, and the menu name stays exactly the CL-0 shape
//     local:<machine>/<tool> built from the real name.
//   - dispatch always keys on the real name FIRST (ManifestToolFor). A tag is
//     only ever a way IN to a tool, never a way to confuse two of them.
//
// What collapses (documented, expected, ADR-0067's contract): names differing
// only by case, by surrounding whitespace, or by which separator runs between
// their words. `read_file`, `read-file`, `Read File` and `READ  FILE` are one
// tag, `read-file`. Nothing else merges — ADR-0067 rejected fuzzy/embedding
// synonymy precisely because `file-read` and `file-write` are embedding-close
// and semantically opposite.
//
// What must never follow from a collapse: two DIFFERENT tools on ONE machine
// becoming indistinguishable at dispatch. When one machine's manifest carries
// two real names that collapse to the same tag, that tag is AMBIGUOUS on that
// machine: it resolves nothing and the step is refused (never guessed), while
// either real name still dispatches exactly.

// ContributedCapability is one entry of the contributed capability vocabulary:
// a tag in the ADR-0067 spelling, the owner principal whose fleet supplies it,
// the machine that serves it, and the REAL wire tool names that collapse into
// it (more than one ⇒ the tag is ambiguous on that machine and resolves
// nothing).
type ContributedCapability struct {
	// Tag is the normalized capability — the vocabulary kernel agent
	// capabilities live in (NormalizeCapability, ADR-0067).
	Tag string
	// Owner is the owner principal this capability belongs to. Every entry
	// carries it so a capability can never travel to a routing layer without
	// the D1 invariant travelling with it.
	Owner PrincipalRef
	// Machine is the worker serving it.
	Machine string
	// Tools are the real wire names collapsing into Tag, sorted. len > 1 is
	// the ambiguity above.
	Tools []string
}

// Ambiguous reports whether more than one real tool on this machine collapses
// into the tag, which makes the TAG unusable for selection (either real name
// still dispatches).
func (c ContributedCapability) Ambiguous() bool { return len(c.Tools) > 1 }

// NormalizeManifest derives one worker's contributed capability vocabulary
// from its manifest, deterministically (sorted by tag, real names sorted
// within a tag). It is the D9 entry point: the fleet sources call it when a
// registration lands, so nothing downstream has to remember to normalize, and
// the wire manifest is never mutated.
func NormalizeManifest(reg WorkerRegistration) []ContributedCapability {
	if len(reg.Tools) == 0 {
		return nil
	}
	byTag := make(map[string][]string, len(reg.Tools))
	for _, t := range reg.Tools {
		tag := NormalizeCapability(t.Name)
		if tag == "" {
			continue
		}
		dup := false
		for _, have := range byTag[tag] {
			if have == t.Name {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		byTag[tag] = append(byTag[tag], t.Name)
	}
	if len(byTag) == 0 {
		return nil
	}
	out := make([]ContributedCapability, 0, len(byTag))
	for tag, names := range byTag {
		sort.Strings(names)
		out = append(out, ContributedCapability{Tag: tag, Owner: reg.Owner, Machine: reg.Machine, Tools: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// ManifestToolFor resolves a name against a worker's manifest in the CL-3
// vocabulary, REAL NAME FIRST:
//
//  1. an exact wire-name match wins outright — the menu hands agents real
//     names, and dispatch must always be able to say exactly which tool ran;
//  2. otherwise the name is treated as a capability TAG and matched by
//     normalized form. Exactly one real tool ⇒ that tool. More than one ⇒
//     ambiguous: found is false and ambiguous is true, so the caller refuses
//     rather than picking one of two different tools.
func ManifestToolFor(reg WorkerRegistration, name string) (tool SystemTool, found, ambiguous bool) {
	for _, t := range reg.Tools {
		if t.Name == name {
			return t, true, false
		}
	}
	tag := NormalizeCapability(name)
	if tag == "" {
		return SystemTool{}, false, false
	}
	var matches []SystemTool
	for _, t := range reg.Tools {
		if NormalizeCapability(t.Name) == tag {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return SystemTool{}, false, false
	case 1:
		return matches[0], true, false
	default:
		return SystemTool{}, false, true
	}
}

// manifestTool is the unambiguous half of ManifestToolFor, for the call sites
// that only need "does this worker offer it".
func manifestTool(reg WorkerRegistration, name string) (SystemTool, bool) {
	t, found, _ := ManifestToolFor(reg, name)
	return t, found
}
