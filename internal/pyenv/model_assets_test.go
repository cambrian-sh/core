package pyenv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0157 D7.2.5 (ENG-2072): the pinned model-asset table.

const testAssetName = "en_core_web_sm-3.8.0-py3-none-any.whl"

// withModelAsset adds one model asset to the fixture's pin table; its bytes
// are written to dir (a wheelhouse or mirror) unless dir is "".
func withModelAsset(t *testing.T, f *fixture, dir string, body []byte) ModelAsset {
	t.Helper()
	a := ModelAsset{
		Asset:   Asset{Name: testAssetName, SHA256: sum(body)},
		Project: "en-core-web-sm", Version: "3.8.0",
		BaseURL: "https://example.invalid/models/",
	}
	f.pins.ModelAssets = []ModelAsset{a}
	if dir != "" {
		must(t, os.WriteFile(filepath.Join(dir, testAssetName), body, 0o644))
	}
	return a
}

func TestEmbeddedPinTableModelAssets(t *testing.T) {
	p, err := Pins()
	must(t, err)
	if len(p.ModelAssets) == 0 {
		t.Fatal("the embedded pin table must pin en_core_web_sm (kg_extractor's default svo tier loads it)")
	}
	a := p.ModelAssets[0]
	if a.Project != "en-core-web-sm" || a.Version != "3.8.0" || a.Name != testAssetName ||
		a.SHA256 != "1932429db727d4bff3deed6b34cfc05df17794f4a52eeb26cf8928f7c1a0fb85" ||
		!strings.HasPrefix(a.BaseURL, "https://github.com/explosion/spacy-models/releases/download/") {
		t.Fatalf("en_core_web_sm pin: %+v", a)
	}
	d, err := p.ModelAssetsDigest()
	if err != nil || len(d) != 64 {
		t.Fatalf("digest %q, %v", d, err)
	}
}

func TestModelAssetsDigestIgnoresURLAndRefusesIncompletePins(t *testing.T) {
	a := ModelAsset{Asset: Asset{Name: "m-1.0-py3-none-any.whl", SHA256: strings.Repeat("a", 64)}, Project: "m", Version: "1.0", BaseURL: "https://a/"}
	p1 := PinTable{ModelAssets: []ModelAsset{a}}
	b := a
	b.BaseURL = "https://mirror.internal/"
	p2 := PinTable{ModelAssets: []ModelAsset{b}}
	d1, _ := p1.ModelAssetsDigest()
	d2, _ := p2.ModelAssetsDigest()
	if d1 != d2 {
		t.Fatal("a different download URL for the same bytes must not change the generation")
	}
	if d, err := (PinTable{}).ModelAssetsDigest(); d != "" || err != nil {
		t.Fatalf("empty table: %q %v", d, err)
	}
	for name, bad := range map[string]ModelAsset{
		"no project": {Asset: a.Asset, Version: "1.0"},
		"short hash": {Asset: Asset{Name: a.Name, SHA256: "abc"}, Project: "m", Version: "1.0"},
		"not wheel":  {Asset: Asset{Name: "m.tar.gz", SHA256: a.SHA256}, Project: "m", Version: "1.0"},
	} {
		if _, err := (PinTable{ModelAssets: []ModelAsset{bad}}).ModelAssetsDigest(); err == nil {
			t.Errorf("%s: want refusal", name)
		}
	}
}

// assetRecorder wraps the fixture runner and keeps the -r file contents of
// every install (the builder deletes them afterwards).
type assetRecorder struct {
	f        *fixture
	installs []string // each install's requirements file contents
	args     []string // each install's argument line
}

func (r *assetRecorder) run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	for i, a := range args {
		if a == "-r" && i+1 < len(args) {
			b, _ := os.ReadFile(args[i+1])
			r.installs = append(r.installs, string(b))
			r.args = append(r.args, strings.Join(args, " "))
		}
	}
	return r.f.run(ctx, env, name, args...)
}

func TestBuildInstallsModelAssetsAfterBaseLockFromWheelhouseOffline(t *testing.T) {
	f := newFixture(t)
	wheels := t.TempDir()
	body := []byte("fake en_core_web_sm wheel")
	withModelAsset(t, f, wheels, body)
	b := f.builder(t)
	b.Sources.Wheelhouse = []string{wheels} // offline; the asset is only in the wheelhouse
	rec := &assetRecorder{f: f}
	b.run = rec.run

	res, err := b.Build(context.Background())
	must(t, err)
	if len(rec.installs) != 2 {
		t.Fatalf("want base lock then model assets, got %d installs: %v", len(rec.installs), rec.args)
	}
	if !strings.Contains(rec.installs[0], "six==1.17.0") || strings.Contains(rec.installs[0], "en-core-web-sm") {
		t.Fatalf("first install must be the base lock alone: %q", rec.installs[0])
	}
	stored := filepath.Join(f.root, dirDownloads, testAssetName)
	want := "en-core-web-sm @ file://"
	if !strings.HasPrefix(rec.installs[1], want) || !strings.Contains(rec.installs[1], "--hash=sha256:"+sum(body)) ||
		!strings.Contains(rec.installs[1], filepath.ToSlash(stored)) {
		t.Fatalf("asset requirements %q must pin the verified copy in downloads/ by digest", rec.installs[1])
	}
	for _, flag := range []string{"--no-deps", "--require-hashes", "--only-binary :all:", "--offline", "--no-index"} {
		if !strings.Contains(rec.args[1], flag) {
			t.Errorf("asset install %q lacks %s", rec.args[1], flag)
		}
	}
	if got, _ := fileSHA256(stored); got != sum(body) {
		t.Fatal("the verified asset must be kept in downloads/ for rebuilds")
	}
	var m Marker
	must(t, readJSON(filepath.Join(res.Generation, fileMarker), &m))
	if m.Packages != 4 || m.Inputs.ModelAssetsSHA256 == "" {
		t.Fatalf("marker must count the asset and record its table digest: %+v", m)
	}
	st, err := b.Check(context.Background())
	must(t, err)
	if st.State != StateHealthy {
		t.Fatalf("check: %+v", st)
	}
}

func TestModelAssetHashMismatchNamesBothDigests(t *testing.T) {
	f := newFixture(t)
	wheels := t.TempDir()
	withModelAsset(t, f, "", nil)
	f.pins.ModelAssets[0].SHA256 = sum([]byte("pinned bytes"))
	must(t, os.WriteFile(filepath.Join(wheels, testAssetName), []byte("tampered bytes"), 0o644))
	b := f.builder(t)
	b.Sources.Wheelhouse = []string{wheels}
	_, err := b.Build(context.Background())
	var mm *HashMismatchError
	if !errors.As(err, &mm) || mm.Want != sum([]byte("pinned bytes")) || mm.Got != sum([]byte("tampered bytes")) {
		t.Fatalf("want a mismatch naming both digests, got %v", err)
	}
	for _, s := range []string{sum([]byte("pinned bytes")), sum([]byte("tampered bytes")), "en-core-web-sm", testAssetName} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q must name %s", err, s)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, dirDownloads, testAssetName)); !os.IsNotExist(err) {
		t.Fatal("a mismatched asset must not be kept")
	}
	if _, err := os.Stat(filepath.Join(f.root, fileCurrent)); !os.IsNotExist(err) {
		t.Fatal("a failed build must not name a generation current")
	}
}

func TestModelAssetOfflineWithoutTheAssetFailsNamingIt(t *testing.T) {
	f := newFixture(t)
	withModelAsset(t, f, "", []byte("x")) // not in the wheelhouse, no mirror
	b := f.builder(t)
	_, err := b.Build(context.Background())
	if !errors.Is(err, ErrOfflineMissing) {
		t.Fatalf("want ErrOfflineMissing, got %v", err)
	}
	for _, s := range []string{"en-core-web-sm", "3.8.0", testAssetName} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q must name %s", err, s)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, "pip install") {
			t.Fatalf("nothing may be installed before every asset is verified: %v", f.calls)
		}
	}
}

func TestModelAssetFromMirrorOnline(t *testing.T) {
	f := newFixture(t)
	mirror := t.TempDir()
	body := []byte("mirrored model wheel")
	withModelAsset(t, f, mirror, body)
	srv := httptest.NewServer(http.FileServer(http.Dir(mirror)))
	defer srv.Close()
	b := f.builder(t)
	b.Sources.Offline = false
	b.Sources.AssetMirror = srv.URL + "/"
	b.HTTPClient = srv.Client()
	_, err := b.Build(context.Background())
	must(t, err)
	if got, _ := fileSHA256(filepath.Join(f.root, dirDownloads, testAssetName)); got != sum(body) {
		t.Fatal("asset not fetched from the mirror")
	}
}

func TestModelAssetChangeIsANewGenerationAndMarksTheOldStale(t *testing.T) {
	f := newFixture(t)
	wheels := t.TempDir()
	b := f.builder(t) // no model assets
	b.Sources.Wheelhouse = []string{wheels}
	first, err := b.Build(context.Background())
	must(t, err)

	body := []byte("model v1")
	withModelAsset(t, f, wheels, body)
	b2 := f.builder(t)
	b2.Sources.Wheelhouse = []string{wheels}
	st, err := b2.Check(context.Background())
	must(t, err)
	if st.State != StateStale || !st.Usable() || !strings.Contains(st.Reason, "model assets none") {
		t.Fatalf("a generation built without the asset table must read stale and usable: %+v", st)
	}
	second, err := b2.Build(context.Background())
	must(t, err)
	if second.Generation == first.Generation {
		t.Fatal("adding a model asset must make a new generation")
	}

	// A new asset version (new bytes, new digest) is another generation.
	body2 := []byte("model v2")
	must(t, os.WriteFile(filepath.Join(wheels, "en_core_web_sm-3.8.1-py3-none-any.whl"), body2, 0o644))
	f.pins.ModelAssets = []ModelAsset{{Asset: Asset{Name: "en_core_web_sm-3.8.1-py3-none-any.whl", SHA256: sum(body2)}, Project: "en-core-web-sm", Version: "3.8.1", BaseURL: "https://example.invalid/"}}
	b3 := f.builder(t)
	b3.Sources.Wheelhouse = []string{wheels}
	if st, _ := b3.Check(context.Background()); st.State != StateStale {
		t.Fatalf("a changed asset must mark the current generation stale: %+v", st)
	}
	third, err := b3.Build(context.Background())
	must(t, err)
	if third.Generation == second.Generation || third.Generation == first.Generation {
		t.Fatal("a changed model asset must make a new generation")
	}
}
