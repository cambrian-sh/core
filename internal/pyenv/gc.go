package pyenv

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Inventory and garbage collection of the env root (ADR-0157 D7.12, P5).
//
// Every generation under base/ and layers/ is immutable and content-named, so a
// switch (a new base build, an agent's new layer) leaves the old one on disk.
// GC deletes the ones nothing can reach any more:
//
//   - base generations: keep the current one (current.json), every one a live
//     instance runs on, and the gc.keep_previous newest others (rollback);
//   - layers: per agent keep the layer the package service handed it last, the
//     newest layer built on each kept base generation (so a rollback finds the
//     agent's packages, and a layer renamed into place a moment before it is
//     handed out is not raced), and every layer a live instance runs on;
//     layers of an agent the kernel no longer knows go (D7.10.3);
//   - downloads/: archives no pin table entry names (any platform);
//   - installer/: extracted installers of other versions;
//   - cache/: untouched by default — it holds the wheels approved locks need
//     for an offline reinstall; gc.prune_cache runs the pinned installer's own
//     `cache prune` (unreferenced entries only).
//
// A build in progress (a .build-* or .part-* entry) is never touched, and
// neither is a base generation built less than Policy.MinAge ago (a build
// renames it into place a moment before current.json names it). Layers need
// no such grace: the package service holds its layer decisions for the whole
// run (KeepSet.Release), so a layer is never chosen and deleted at once.

// GCPolicy is execution.agents.python.gc.
type GCPolicy struct {
	// KeepPrevious is how many previous base generations to keep for rollback
	// besides the current one and the ones in use (gc.keep_previous, default 1).
	KeepPrevious int
	// PruneCache runs the installer's `cache prune` over cache/ (gc.prune_cache).
	PruneCache bool
	// MinAge protects base generations built this recently (0 ⇒
	// DefaultGCMinAge; negative ⇒ none, for tests).
	MinAge time.Duration
}

// DefaultGCMinAge protects a generation a concurrent build just renamed into
// place before current.json or the package service names it.
const DefaultGCMinAge = 2 * time.Minute

// KeepSet is what the kernel's live state protects from one GC run.
type KeepSet struct {
	// InUse are the generation directories (base homes and layer dirs) live
	// agent instances run on.
	InUse []string
	// Layers is each agent's current layer directory (the one its last package
	// check handed it); "" when that check handed it none. An agent not checked
	// since boot is absent.
	Layers map[string]string
	// KnownAgents are the agents the kernel knows; nil ⇒ unknown, and no layer
	// is collected for belonging to a removed agent.
	KnownAgents map[string]bool
	// Release, when set, is called once the run has finished deleting (the
	// package service holds its layer decisions until then).
	Release func()
}

// GCItem is one deleted (or kept) generation or file.
type GCItem struct {
	Kind   string `json:"kind"` // base | layer | download | installer | cache
	Path   string `json:"path"`
	Agent  string `json:"agent,omitempty"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
}

// GCReport is one GC run's result.
type GCReport struct {
	Removed    []GCItem  `json:"removed"`
	Kept       []GCItem  `json:"kept"`
	FreedBytes int64     `json:"freed_bytes"`
	Errors     []string  `json:"errors,omitempty"`
	At         time.Time `json:"at"`
}

// BaseGeneration is one base generation on disk.
type BaseGeneration struct {
	Dir     string    `json:"dir"` // directory name under base/
	Path    string    `json:"path"`
	Current bool      `json:"current"`
	Healthy bool      `json:"healthy"` // the marker reads as a base marker for this directory
	BuiltAt time.Time `json:"built_at"`
	Source  string    `json:"source,omitempty"`
	Bytes   int64     `json:"bytes"`
}

// LayerGeneration is one agent layer on disk.
type LayerGeneration struct {
	Agent    string                   `json:"agent"`
	Dir      string                   `json:"dir"` // the layer id (directory name)
	Path     string                   `json:"path"`
	Base     string                   `json:"base"`
	Healthy  bool                     `json:"healthy"`
	BuiltAt  time.Time                `json:"built_at"`
	Declared []string                 `json:"declared,omitempty"`
	Entries  []domain.PythonLockEntry `json:"entries,omitempty"`
	Bytes    int64                    `json:"bytes"`
}

// Inventory is the env root as it is on disk, with sizes (the operator plane's
// per-root size, D7.12).
type Inventory struct {
	Root           string            `json:"root"`
	CurrentBase    string            `json:"current_base,omitempty"`
	Bases          []BaseGeneration  `json:"bases"`
	Layers         []LayerGeneration `json:"layers"`
	TotalBytes     int64             `json:"total_bytes"`
	CacheBytes     int64             `json:"cache_bytes"`
	DownloadsBytes int64             `json:"downloads_bytes"`
}

// treeBytes sums the regular files under p (0 when it is missing).
func treeBytes(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries count as 0
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (b *Builder) currentBase() string {
	var cur Current
	if err := readJSON(filepath.Join(b.Root, fileCurrent), &cur); err != nil {
		return ""
	}
	return cur.Base
}

// listBases reads base/ (build directories skipped). withSize walks each tree.
func (b *Builder) listBases(withSize bool) []BaseGeneration {
	cur := b.currentBase()
	ents, _ := os.ReadDir(filepath.Join(b.Root, dirBase))
	var out []BaseGeneration
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(b.Root, dirBase, e.Name())
		g := BaseGeneration{Dir: e.Name(), Path: p, Current: e.Name() == cur}
		var m Marker
		if err := readJSON(filepath.Join(p, fileMarker), &m); err == nil {
			g.BuiltAt, g.Source = m.BuiltAt, m.Source
			g.Healthy = m.Kind == "base" && len(m.ID) >= 16 && m.ID[:16] == e.Name() && m.Health == "ok"
		}
		if g.BuiltAt.IsZero() {
			if fi, err := e.Info(); err == nil {
				g.BuiltAt = fi.ModTime().UTC()
			}
		}
		if withSize {
			g.Bytes = treeBytes(p)
		}
		out = append(out, g)
	}
	// Newest first.
	sort.Slice(out, func(i, j int) bool { return out[i].BuiltAt.After(out[j].BuiltAt) })
	return out
}

// listLayers reads layers/<agent dir>/<id>/ (build directories skipped).
func (b *Builder) listLayers(withSize bool) []LayerGeneration {
	agents, _ := os.ReadDir(filepath.Join(b.Root, dirLayers))
	var out []LayerGeneration
	for _, a := range agents {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") {
			continue
		}
		gens, _ := os.ReadDir(filepath.Join(b.Root, dirLayers, a.Name()))
		for _, g := range gens {
			if !g.IsDir() || strings.HasPrefix(g.Name(), ".") {
				continue
			}
			p := filepath.Join(b.Root, dirLayers, a.Name(), g.Name())
			l := LayerGeneration{Dir: g.Name(), Path: p}
			var m LayerMarker
			if err := readJSON(filepath.Join(p, fileMarker), &m); err == nil && m.Kind == layerMarkerKd {
				l.Agent, l.Base, l.BuiltAt, l.Declared, l.Entries = m.Agent, m.Base, m.BuiltAt, m.Declared, m.Entries
				l.Healthy = m.Health == "ok" && len(m.ID) >= 16 && m.ID[:16] == g.Name()
			}
			if l.BuiltAt.IsZero() {
				if fi, err := g.Info(); err == nil {
					l.BuiltAt = fi.ModTime().UTC()
				}
			}
			if withSize {
				l.Bytes = treeBytes(p)
			}
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].BuiltAt.After(out[j].BuiltAt)
	})
	return out
}

// Inventory lists the env root's generations with their sizes. It walks every
// tree, so it costs a few hundred ms on a multi-GB root.
func (b *Builder) Inventory() Inventory {
	inv := Inventory{Root: b.Root, CurrentBase: b.currentBase(), Bases: b.listBases(true), Layers: b.listLayers(true)}
	inv.CacheBytes = treeBytes(filepath.Join(b.Root, dirCache))
	inv.DownloadsBytes = treeBytes(filepath.Join(b.Root, dirDownloads))
	inv.TotalBytes = treeBytes(b.Root)
	return inv
}

func cleanSet(paths []string) map[string]bool {
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p != "" {
			out[strings.ToLower(filepath.Clean(p))] = true
		}
	}
	return out
}

func inSet(set map[string]bool, p string) bool { return set[strings.ToLower(filepath.Clean(p))] }

// Collect runs one GC pass over the env root (D7.12) with the given policy and
// live state. Deletion errors are reported, never fatal: a generation that
// cannot be removed now (a file still open on Windows) is tried again on the
// next run.
func (b *Builder) Collect(ctx context.Context, p GCPolicy, keep KeepSet) GCReport {
	if keep.Release != nil {
		defer keep.Release()
	}
	minAge := p.MinAge
	if minAge == 0 {
		minAge = DefaultGCMinAge
	}
	now := time.Now().UTC()
	young := func(t time.Time) bool { return minAge > 0 && !t.IsZero() && now.Sub(t) < minAge }
	rep := GCReport{At: now}
	inUse := cleanSet(keep.InUse)
	remove := func(it GCItem) {
		if err := ctx.Err(); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: not removed: %v", it.Path, err))
			return
		}
		it.Bytes = treeBytes(it.Path)
		if fi, err := os.Stat(it.Path); err == nil && fi.Mode().IsRegular() {
			it.Bytes = fi.Size()
		}
		if err := os.RemoveAll(it.Path); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", it.Path, err))
			return
		}
		rep.Removed = append(rep.Removed, it)
		rep.FreedBytes += it.Bytes
	}
	kept := func(it GCItem) { rep.Kept = append(rep.Kept, it) }

	// Base generations (newest first).
	keepPrev := p.KeepPrevious
	if keepPrev < 0 {
		keepPrev = 0
	}
	keptBases := map[string]bool{}
	prev := 0
	for _, g := range b.listBases(false) {
		it := GCItem{Kind: "base", Path: g.Path}
		switch {
		case g.Current:
			it.Reason = "current"
		case inSet(inUse, g.Path):
			it.Reason = "in use by a live agent instance"
		case young(g.BuiltAt):
			it.Reason = "built less than " + minAge.String() + " ago"
		case g.Healthy && prev < keepPrev:
			prev++
			it.Reason = fmt.Sprintf("previous generation kept for rollback (gc.keep_previous=%d)", keepPrev)
		default:
			it.Reason = "not current, not in use, beyond gc.keep_previous"
			remove(it)
			continue
		}
		keptBases[g.Dir] = true
		kept(it)
	}

	// Layers (per agent, newest first).
	current := map[string]bool{}
	for _, d := range keep.Layers {
		current[strings.ToLower(filepath.Clean(d))] = true
	}
	newestOnBase := map[string]bool{} // agent|base already has its newest kept
	for _, l := range b.listLayers(false) {
		it := GCItem{Kind: "layer", Path: l.Path, Agent: l.Agent}
		key := l.Agent + "|" + l.Base
		known := keep.KnownAgents == nil || l.Agent == "" || keep.KnownAgents[l.Agent]
		switch {
		case current[strings.ToLower(filepath.Clean(l.Path))]:
			it.Reason = "the agent's current layer"
		case inSet(inUse, l.Path):
			it.Reason = "in use by a live agent instance"
		case !known:
			it.Reason = "the agent is no longer registered"
			remove(it)
			continue
		case l.Healthy && keptBases[l.Base] && !newestOnBase[key] && !b.isCurrentBase(l.Base):
			// A kept previous base keeps the agent's newest layer on it, so a
			// rollback finds the agent's packages.
			newestOnBase[key] = true
			it.Reason = "the agent's newest layer on a kept previous base generation"
		case l.Healthy && keptBases[l.Base] && !newestOnBase[key] && !hasAgent(keep.Layers, l.Agent):
			// On the current base the agent's current layer is the one to keep;
			// an agent not checked since boot has none on record yet, so its
			// newest layer (most likely the one its next spawn is handed) stays
			// rather than forcing a reinstall.
			newestOnBase[key] = true
			it.Reason = "the agent's newest layer (not checked since boot)"
		default:
			it.Reason = "superseded and not in use"
			remove(it)
			continue
		}
		kept(it)
	}

	// Empty agent directories left behind.
	if ents, err := os.ReadDir(filepath.Join(b.Root, dirLayers)); err == nil {
		for _, a := range ents {
			p := filepath.Join(b.Root, dirLayers, a.Name())
			if sub, err := os.ReadDir(p); err == nil && len(sub) == 0 {
				_ = os.Remove(p)
			}
		}
	}

	// downloads/: archives no pin table entry names.
	named := map[string]bool{}
	for _, a := range b.Pins.Python.Assets {
		named[a.Name] = true
	}
	for _, a := range b.Pins.Installer.Assets {
		named[a.Name] = true
	}
	for _, a := range b.Pins.ModelAssets {
		named[a.Name] = true
	}
	if ents, err := os.ReadDir(filepath.Join(b.Root, dirDownloads)); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".") || named[e.Name()] {
				continue
			}
			remove(GCItem{Kind: "download", Path: filepath.Join(b.Root, dirDownloads, e.Name()), Reason: "no pin table entry names it"})
		}
	}
	// installer/: other versions or platforms than the pinned one.
	want := b.Pins.Installer.Version + "-" + b.Platform
	if ents, err := os.ReadDir(filepath.Join(b.Root, dirInstaller)); err == nil {
		for _, e := range ents {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == want {
				continue
			}
			remove(GCItem{Kind: "installer", Path: filepath.Join(b.Root, dirInstaller, e.Name()), Reason: "not the pinned installer"})
		}
	}
	if p.PruneCache {
		if err := b.pruneCache(ctx); err != nil {
			rep.Errors = append(rep.Errors, "cache prune: "+err.Error())
		}
	}
	return rep
}

func hasAgent(m map[string]string, agent string) bool {
	_, ok := m[agent]
	return ok
}

func (b *Builder) isCurrentBase(dir string) bool { return dir != "" && dir == b.currentBase() }

// pruneCache runs the pinned installer's own cache pruning over cache/: it
// removes entries no longer referenced (uv's definition), never a network call.
func (b *Builder) pruneCache(ctx context.Context) error {
	uv := filepath.Join(b.Root, dirInstaller, b.Pins.Installer.Version+"-"+b.Platform, installerBinary(b.Platform))
	if _, err := os.Stat(uv); err != nil {
		return fmt.Errorf("the pinned installer is not extracted yet (%s)", uv)
	}
	cache := filepath.Join(b.Root, dirCache)
	if _, err := os.Stat(cache); err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := b.runner()(cctx, installerEnv(), uv, "cache", "prune", "--cache-dir", cache, "--no-config")
	if err != nil {
		return fmt.Errorf("%w: %s", err, lastLines(out, 4))
	}
	return nil
}

// Collector runs GC in the background: once when the env is first ready, and
// again on every Trigger (a base switch, a layer change, a live instance
// gone). Triggers coalesce; a run never overlaps another. It owns at most one
// goroutine (Start/Stop, AGENTS.md §3.3).
type Collector struct {
	b      *Builder
	policy GCPolicy
	keep   func(ctx context.Context) KeepSet
	ready  <-chan struct{}

	runMu   sync.Mutex // one Collect at a time (background or Run)
	trigger chan struct{}
	mu      sync.Mutex
	last    GCReport
	cancel  context.CancelFunc
	done    chan struct{}
}

// NewCollector builds a collector. keep is read at the start of every run;
// ready (nil ⇒ run at once) is closed when the env is first usable.
func NewCollector(b *Builder, policy GCPolicy, keep func(ctx context.Context) KeepSet, ready <-chan struct{}) (*Collector, error) {
	if b == nil || keep == nil {
		return nil, fmt.Errorf("pyenv: a GC collector needs the env builder and the live keep set")
	}
	return &Collector{b: b, policy: policy, keep: keep, ready: ready, trigger: make(chan struct{}, 1)}, nil
}

// Trigger asks for a run soon (non-blocking; coalesced).
func (c *Collector) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run collects now, synchronously (the operator's prune command).
func (c *Collector) Run(ctx context.Context) GCReport {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	rep := c.b.Collect(ctx, c.policy, c.keep(ctx))
	c.mu.Lock()
	c.last = rep
	c.mu.Unlock()
	if len(rep.Removed) > 0 || len(rep.Errors) > 0 {
		slog.InfoContext(ctx, "ADR-0157 D7.12: sandbox Python env GC", "removed", len(rep.Removed),
			"freed_bytes", rep.FreedBytes, "kept", len(rep.Kept), "errors", rep.Errors)
	}
	return rep
}

// Last is the most recent run's report (zero before the first).
func (c *Collector) Last() GCReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Start runs the background loop: the boot GC once ready is closed, then one
// run per coalesced trigger.
func (c *Collector) Start(ctx context.Context) {
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.cancel, c.done = cancel, make(chan struct{})
	done := c.done
	c.mu.Unlock()
	go func() {
		defer close(done)
		if c.ready != nil {
			select {
			case <-c.ready:
			case <-cctx.Done():
				return
			}
		}
		c.Run(cctx) // GC at boot (D7.12)
		for {
			select {
			case <-cctx.Done():
				return
			case <-c.trigger:
				c.Run(cctx)
			}
		}
	}()
}

// Stop ends the background loop and waits for it.
func (c *Collector) Stop() {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
