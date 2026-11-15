package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/cambrian-sh/core/internal/config"
)

func TestEncrypt_RoundTripAndWrongPassphrase(t *testing.T) {
	pt := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	sealed, err := Encrypt(pt, []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, pt) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := Decrypt(sealed, []byte("correct horse"))
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := Decrypt(sealed, []byte("wrong")); err == nil {
		t.Fatal("a wrong passphrase must fail")
	}
	if _, err := Encrypt(pt, nil); err == nil {
		t.Fatal("an empty passphrase must be refused")
	}
}

func TestDecryptFile_NeverOverwrites(t *testing.T) {
	d := t.TempDir()
	src, enc, dst := filepath.Join(d, "k"), filepath.Join(d, "enc", SecretKeyFileName), filepath.Join(d, "out")
	if err := os.WriteFile(src, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EncryptFile(src, enc, []byte("p")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("live key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(enc, dst, []byte("p")); err == nil {
		t.Fatal("DecryptFile overwrote an existing key")
	}
	if b, _ := os.ReadFile(dst); string(b) != "live key" {
		t.Fatal("existing key changed")
	}
}

func run(name string, kind string, created time.Time, complete bool) RunInfo {
	return RunInfo{Name: name, Kind: kind, Created: created, Complete: complete}
}

func TestKeep_GrandfatherFatherSon(t *testing.T) {
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	var runs []RunInfo
	// One nightly per day for 120 days, plus a second (manual) run today.
	for i := 0; i < 120; i++ {
		c := now.AddDate(0, 0, -i)
		runs = append(runs, run(RunName(c, KindNightly), KindNightly, c, true))
	}
	manual := now.Add(time.Hour)
	runs = append(runs, run(RunName(manual, KindManual), KindManual, manual, true))
	pre := now.AddDate(0, 0, -200)
	runs = append(runs, run(RunName(pre, KindPreMigration), KindPreMigration, pre, true))
	oldIncomplete := now.AddDate(0, 0, -3)
	runs = append(runs, run(RunName(oldIncomplete, KindNightly)+"x", KindNightly, oldIncomplete, false))
	freshIncomplete := now.Add(-time.Hour)
	runs = append(runs, run("fresh-incomplete", KindNightly, freshIncomplete, false))

	keep := Keep(runs, Retention{Daily: 7, Weekly: 4, Monthly: 3}, now.Add(2*time.Hour))

	daily := 0
	for i := 0; i < 7; i++ {
		day := now.AddDate(0, 0, -i)
		if keep[RunName(day, KindNightly)] || (i == 0 && keep[RunName(manual, KindManual)]) {
			daily++
		}
	}
	if daily != 7 {
		t.Fatalf("want one kept run for each of the last 7 days, got %d", daily)
	}
	if !keep[RunName(manual, KindManual)] {
		t.Fatal("the newest run of today (the manual one) must be kept")
	}
	if !keep[RunName(pre, KindPreMigration)] {
		t.Fatal("pre-migration snapshots are never rotated away")
	}
	if keep[RunName(oldIncomplete, KindNightly)+"x"] {
		t.Fatal("an incomplete run older than a day must be pruned")
	}
	if !keep["fresh-incomplete"] {
		t.Fatal("an incomplete run younger than a day may be in progress and must be kept")
	}
	// Count kept rotated runs: at most 7 + 4 + 3 distinct, and the oldest kept is ~3 months back.
	n := 0
	var oldest time.Time
	for _, r := range runs {
		if keep[r.Name] && rotated(r.Kind) && r.Complete {
			n++
			if oldest.IsZero() || r.Created.Before(oldest) {
				oldest = r.Created
			}
		}
	}
	if n > 14 || n < 10 {
		t.Fatalf("kept %d rotated runs, want between 10 and 14 (7 daily + 4 weekly + 3 monthly, overlapping)", n)
	}
	// 3 monthly = the newest run of November, October and September.
	if want := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC); !oldest.Equal(want) {
		t.Fatalf("oldest kept run %s, want %s (newest of the third month back)", oldest, want)
	}
}

func TestPrune_RemovesRunAndItsKeyFolder_IgnoresForeignFolders(t *testing.T) {
	root, keys := t.TempDir(), t.TempDir()
	now := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	mk := func(base, name string, manifest bool) {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if manifest {
			if err := os.WriteFile(filepath.Join(d, "manifest.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var names []string
	for i := 0; i < 10; i++ {
		n := RunName(now.AddDate(0, 0, -i), KindNightly)
		names = append(names, n)
		mk(root, n, true)
		mk(keys, n, false)
	}
	mk(root, "not-a-run", false)
	removed, err := Prune(root, keys, Retention{Daily: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 7 {
		t.Fatalf("removed %v, want 7", removed)
	}
	for i, n := range names {
		_, errRun := os.Stat(filepath.Join(root, n))
		_, errKey := os.Stat(filepath.Join(keys, n))
		if kept := i < 3; kept != (errRun == nil) || kept != (errKey == nil) {
			t.Fatalf("%s: run present=%v key present=%v, want %v", n, errRun == nil, errKey == nil, kept)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "not-a-run")); err != nil {
		t.Fatal("prune touched a folder that is not a run")
	}
}

func makeBolt(t *testing.T, p string) {
	t.Helper()
	db, err := bbolt.Open(p, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("b"))
		if err != nil {
			return err
		}
		return b.Put([]byte("k"), []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCopyBbolt_ConsistentAndWhileHeld(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "agents.db")
	makeBolt(t, src)
	note, err := copyBbolt(src, filepath.Join(d, "out", "a.db"))
	if err != nil || note != "consistent" {
		t.Fatalf("free lock: note=%q err=%v", note, err)
	}
	if err := checkBbolt(filepath.Join(d, "out", "a.db")); err != nil {
		t.Fatal(err)
	}
	// Held by a writer (the running kernel): a checked hot copy.
	held, err := bbolt.Open(src, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	note, err = copyBbolt(src, filepath.Join(d, "out", "b.db"))
	if err != nil || !strings.Contains(note, "hot copy") {
		t.Fatalf("held lock: note=%q err=%v", note, err)
	}
	if !isBbolt(src) || isBbolt(filepath.Join(d, "nope")) {
		t.Fatal("isBbolt")
	}
}

func TestVerify_DetectsTamperingAndIncompleteRuns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "db.dump"), []byte("PGDMP fake archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), dir, nil); err == nil {
		t.Fatal("a folder without a manifest is not a backup")
	}
	f, err := describe(dir, "db.dump", FileDump)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(dir, &Manifest{Format: ManifestFormat, Kind: KindManual, Files: []File{*f}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), dir, nil); err != nil {
		t.Fatalf("intact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "db.dump"), []byte("PGDMP fake archive bytez"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), dir, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered dump must fail verification, got %v", err)
	}
}

func TestRun_RefusesSecretKeyWithoutPassphraseBeforeAnyWork(t *testing.T) {
	d := t.TempDir()
	key := filepath.Join(d, "secret.key")
	if err := os.WriteFile(key, []byte("00"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(d, "backups")
	_, _, err := Run(context.Background(), Options{Root: root, KeyRoot: filepath.Join(d, "keys"), Kind: KindNightly,
		SecretKeyPath: key, PassphraseEnv: "CAMBRIAN_BACKUP_PASSPHRASE"})
	if err == nil || !strings.Contains(err.Error(), "CAMBRIAN_BACKUP_PASSPHRASE") || !strings.Contains(err.Error(), "--no-secret-key") {
		t.Fatalf("want a refusal naming the env var and the override, got %v", err)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatal("the refusal must come before anything is written")
	}
	_, _, err = Run(context.Background(), Options{Root: root, KeyRoot: filepath.Join(root, "keys"), Kind: KindNightly})
	if err == nil || !strings.Contains(err.Error(), "kept apart") {
		t.Fatalf("a key root inside the backup root must be refused, got %v", err)
	}
}

func TestResolvePaths(t *testing.T) {
	base := t.TempDir()
	p, err := ResolvePaths(config.DefaultBackupConfig(), base)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != filepath.Join(base, "backups") || p.KeyRoot != filepath.Join(base, "backups-keys") {
		t.Fatalf("defaults resolved to %+v", p)
	}
	b := config.DefaultBackupConfig()
	b.KeyDir = "backups/keys"
	if _, err := ResolvePaths(b, base); err == nil {
		t.Fatal("key_dir inside dir must be refused")
	}
}

func TestCompareCounts(t *testing.T) {
	want := []TableCount{{"public.a", 3}, {"public.b", 5}, {"public.c", 1}}
	got := []TableCount{{"public.a", 3}, {"public.b", 4}, {"public.d", 9}}
	m := CompareCounts(want, got)
	if len(m) != 3 {
		t.Fatalf("mismatches %v", m)
	}
	if len(CompareCounts(want, want)) != 0 {
		t.Fatal("identical counts must match")
	}
}
