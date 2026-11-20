package pyenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gcFixture lays out an env root by hand: base generations and layers with
// markers, a current.json, downloads and installer extractions.
type gcFixture struct {
	t    *testing.T
	b    *Builder
	root string
}

func newGCFixture(t *testing.T) *gcFixture {
	t.Helper()
	root := t.TempDir()
	b := &Builder{Root: root, Platform: "linux-x86_64-gnu", Pins: PinTable{
		Python:    Release{Version: "3.12", Assets: map[string]Asset{"linux-x86_64-gnu": {Name: "cpython.tar.gz"}, "windows-x86_64": {Name: "cpython-win.tar.gz"}}},
		Installer: Release{Version: "0.5.0", Assets: map[string]Asset{"linux-x86_64-gnu": {Name: "uv.tar.gz"}}},
	}}
	return &gcFixture{t: t, b: b, root: root}
}

func gcID(dir string) string { return dir + strings.Repeat("0", 64-len(dir)) }

func (f *gcFixture) base(dir string, built time.Time) string {
	f.t.Helper()
	p := filepath.Join(f.root, dirBase, dir)
	if err := os.MkdirAll(filepath.Join(p, "lib"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "lib", "x.py"), []byte("payload"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(p, fileMarker), Marker{Kind: "base", ID: gcID(dir), Health: "ok", BuiltAt: built}); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *gcFixture) layer(agent, dir, base string, built time.Time) string {
	f.t.Helper()
	p := filepath.Join(f.root, dirLayers, agentDir(agent), dir)
	if err := os.MkdirAll(filepath.Join(p, layerSite), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(p, fileMarker), LayerMarker{Kind: layerMarkerKd, ID: gcID(dir), Agent: agent, Base: base, Health: "ok", BuiltAt: built}); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *gcFixture) current(dir string) {
	f.t.Helper()
	if err := writeCurrent(f.root, Current{Base: dir}); err != nil {
		f.t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

var gcNoGrace = GCPolicy{KeepPrevious: 1, MinAge: -1}

// D7.12 / P5 unit: GC keeps the current base, every base a live instance runs
// on, and keep_previous previous ones; the rest go.
func TestCollect_KeepsCurrentInUseAndKeepPreviousBases(t *testing.T) {
	f := newGCFixture(t)
	now := time.Now().UTC()
	b1 := f.base("1111111111111111", now.Add(-5*time.Hour)) // oldest, in use
	b2 := f.base("2222222222222222", now.Add(-4*time.Hour)) // beyond keep_previous
	b3 := f.base("3333333333333333", now.Add(-3*time.Hour)) // the one previous kept
	b4 := f.base("4444444444444444", now.Add(-1*time.Hour)) // current
	f.current("4444444444444444")

	rep := f.b.Collect(context.Background(), gcNoGrace, KeepSet{InUse: []string{b1}})
	if !exists(b1) || !exists(b3) || !exists(b4) {
		t.Fatalf("kept set wrong: b1=%v b3=%v b4=%v (report %+v)", exists(b1), exists(b3), exists(b4), rep)
	}
	if exists(b2) {
		t.Fatalf("base beyond keep_previous survived: %+v", rep)
	}
	if len(rep.Removed) != 1 || rep.Removed[0].Path != b2 || rep.FreedBytes <= 0 {
		t.Fatalf("report: %+v", rep)
	}

	// keep_previous 0: the previous one goes too, the in-use one stays.
	rep = f.b.Collect(context.Background(), GCPolicy{KeepPrevious: 0, MinAge: -1}, KeepSet{InUse: []string{b1}})
	if exists(b3) || !exists(b1) || !exists(b4) {
		t.Fatalf("keep_previous=0: b1=%v b3=%v b4=%v (%+v)", exists(b1), exists(b3), exists(b4), rep)
	}
	// The instance on b1 is gone: b1 goes at the next run.
	f.b.Collect(context.Background(), GCPolicy{KeepPrevious: 0, MinAge: -1}, KeepSet{})
	if exists(b1) || !exists(b4) {
		t.Fatalf("released base not collected: b1=%v b4=%v", exists(b1), exists(b4))
	}
}

// Layers: the agent's current layer and the in-use ones stay; a superseded
// layer goes once no instance runs on it; a removed agent's layers go.
func TestCollect_Layers(t *testing.T) {
	f := newGCFixture(t)
	now := time.Now().UTC()
	f.base("aaaaaaaaaaaaaaaa", now.Add(-2*time.Hour))
	f.current("aaaaaaaaaaaaaaaa")
	old := f.layer("alpha", "1000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-90*time.Minute))
	cur := f.layer("alpha", "2000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-60*time.Minute))
	gone := f.layer("ghost", "3000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-60*time.Minute))
	unchecked := f.layer("beta", "4000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-60*time.Minute))
	uncheckedOld := f.layer("beta", "5000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-120*time.Minute))
	known := map[string]bool{"alpha": true, "beta": true}

	// A running instance still uses the old layer: nothing of alpha goes.
	f.b.Collect(context.Background(), gcNoGrace, KeepSet{InUse: []string{old}, Layers: map[string]string{"alpha": cur}, KnownAgents: known})
	if !exists(old) || !exists(cur) {
		t.Fatalf("alpha layers: old=%v cur=%v", exists(old), exists(cur))
	}
	if exists(gone) {
		t.Fatal("an unregistered agent's layer survived")
	}
	if !exists(unchecked) || exists(uncheckedOld) {
		t.Fatalf("an agent not checked since boot keeps exactly its newest layer: newest=%v older=%v", exists(unchecked), exists(uncheckedOld))
	}
	// The instance recycled: the old layer goes, the current stays.
	rep := f.b.Collect(context.Background(), gcNoGrace, KeepSet{Layers: map[string]string{"alpha": cur, "beta": ""}, KnownAgents: known})
	if exists(old) || !exists(cur) {
		t.Fatalf("after recycle: old=%v cur=%v (%+v)", exists(old), exists(cur), rep)
	}
	// beta checked and handed no layer: its layers go.
	if exists(unchecked) {
		t.Fatalf("beta has no layer now, its old one survived: %+v", rep)
	}
	if exists(filepath.Join(f.root, dirLayers, "beta")) {
		t.Fatal("empty agent directory left behind")
	}
}

// A previous kept base keeps the agent's newest layer on it (rollback); a
// young base generation and a build in progress are never touched.
func TestCollect_RollbackLayerYoungAndBuilds(t *testing.T) {
	f := newGCFixture(t)
	now := time.Now().UTC()
	f.base("aaaaaaaaaaaaaaaa", now.Add(-3*time.Hour))
	f.base("bbbbbbbbbbbbbbbb", now.Add(-1*time.Hour))
	f.current("bbbbbbbbbbbbbbbb")
	onPrevNew := f.layer("alpha", "1000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-150*time.Minute))
	onPrevOld := f.layer("alpha", "2000000000000000", "aaaaaaaaaaaaaaaa", now.Add(-170*time.Minute))
	cur := f.layer("alpha", "3000000000000000", "bbbbbbbbbbbbbbbb", now.Add(-50*time.Minute))
	young := f.base("cccccccccccccccc", now.Add(-10*time.Second)) // renamed into place, not current yet
	build := filepath.Join(f.root, dirLayers, "alpha", ".build-xyz")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	f.b.Collect(context.Background(), GCPolicy{KeepPrevious: 1}, KeepSet{Layers: map[string]string{"alpha": cur}})
	if !exists(onPrevNew) || exists(onPrevOld) || !exists(cur) || !exists(young) || !exists(build) {
		t.Fatalf("prevNew=%v prevOld=%v cur=%v young=%v build=%v", exists(onPrevNew), exists(onPrevOld), exists(cur), exists(young), exists(build))
	}
}

// downloads/ keeps what any pin table entry names; installer/ keeps the pinned
// version; the cache is untouched without prune_cache.
func TestCollect_DownloadsInstallerCache(t *testing.T) {
	f := newGCFixture(t)
	dl := filepath.Join(f.root, dirDownloads)
	for _, n := range []string{"cpython.tar.gz", "cpython-win.tar.gz", "uv.tar.gz", "cpython-old.tar.gz", ".part-123"} {
		if err := os.MkdirAll(dl, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dl, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"0.5.0-linux-x86_64-gnu", "0.4.0-linux-x86_64-gnu"} {
		if err := os.MkdirAll(filepath.Join(f.root, dirInstaller, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(f.root, dirCache, "wheels-v1")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	f.b.Collect(context.Background(), gcNoGrace, KeepSet{})
	for n, want := range map[string]bool{"cpython.tar.gz": true, "cpython-win.tar.gz": true, "uv.tar.gz": true, "cpython-old.tar.gz": false, ".part-123": true} {
		if exists(filepath.Join(dl, n)) != want {
			t.Errorf("downloads/%s present=%v want %v", n, !want, want)
		}
	}
	if !exists(filepath.Join(f.root, dirInstaller, "0.5.0-linux-x86_64-gnu")) || exists(filepath.Join(f.root, dirInstaller, "0.4.0-linux-x86_64-gnu")) {
		t.Error("installer/: wrong version kept")
	}
	if !exists(cache) {
		t.Error("cache touched without prune_cache")
	}
}

// prune_cache runs the pinned installer's `cache prune` over cache/.
func TestCollect_PruneCacheRunsInstaller(t *testing.T) {
	f := newGCFixture(t)
	uv := filepath.Join(f.root, dirInstaller, "0.5.0-linux-x86_64-gnu", installerBinary(f.b.Platform))
	if err := os.MkdirAll(filepath.Dir(uv), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uv, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, dirCache), 0o755); err != nil {
		t.Fatal(err)
	}
	var got []string
	f.b.run = func(_ context.Context, _ []string, name string, args ...string) (string, error) {
		got = append([]string{name}, args...)
		return "", nil
	}
	rep := f.b.Collect(context.Background(), GCPolicy{KeepPrevious: 1, PruneCache: true, MinAge: -1}, KeepSet{})
	if len(rep.Errors) != 0 || len(got) < 3 || got[0] != uv || got[1] != "cache" || got[2] != "prune" {
		t.Fatalf("prune: %v errors %v", got, rep.Errors)
	}
}

// Inventory reports generations with sizes and the current base.
func TestInventory(t *testing.T) {
	f := newGCFixture(t)
	now := time.Now().UTC()
	f.base("aaaaaaaaaaaaaaaa", now.Add(-time.Hour))
	f.current("aaaaaaaaaaaaaaaa")
	f.layer("alpha", "1000000000000000", "aaaaaaaaaaaaaaaa", now)
	inv := f.b.Inventory()
	if inv.CurrentBase != "aaaaaaaaaaaaaaaa" || len(inv.Bases) != 1 || !inv.Bases[0].Current || !inv.Bases[0].Healthy || inv.Bases[0].Bytes == 0 {
		t.Fatalf("bases: %+v", inv)
	}
	if len(inv.Layers) != 1 || inv.Layers[0].Agent != "alpha" || !inv.Layers[0].Healthy || inv.TotalBytes < inv.Bases[0].Bytes {
		t.Fatalf("layers: %+v", inv)
	}
}

// The collector runs once when ready and again per trigger; Stop ends it.
func TestCollector_BootAndTrigger(t *testing.T) {
	f := newGCFixture(t)
	now := time.Now().UTC()
	f.base("aaaaaaaaaaaaaaaa", now.Add(-3*time.Hour))
	f.base("bbbbbbbbbbbbbbbb", now.Add(-2*time.Hour))
	cur := f.base("cccccccccccccccc", now.Add(-1*time.Hour))
	f.current("cccccccccccccccc")
	runs := make(chan struct{}, 8)
	ready := make(chan struct{})
	c, err := NewCollector(f.b, GCPolicy{KeepPrevious: 0, MinAge: -1}, func(context.Context) KeepSet {
		return KeepSet{Release: func() { runs <- struct{}{} }}
	}, ready)
	if err != nil {
		t.Fatal(err)
	}
	c.Start(context.Background())
	defer c.Stop()
	select {
	case <-runs:
		t.Fatal("collector ran before the env was ready")
	case <-time.After(50 * time.Millisecond):
	}
	close(ready)
	<-runs
	if len(c.Last().Removed) != 2 || !exists(cur) {
		t.Fatalf("boot GC: %+v", c.Last())
	}
	f.base("dddddddddddddddd", now.Add(-4*time.Hour))
	c.Trigger()
	<-runs
	if exists(filepath.Join(f.root, dirBase, "dddddddddddddddd")) {
		t.Fatal("triggered GC did not run")
	}
}
