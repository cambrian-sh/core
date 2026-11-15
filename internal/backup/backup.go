// Package backup backs up, verifies, restores and drills the kernel's durable state
// (T-20261106-decide-the-database-backup-strategy-for, ENG-681; owner decision 2026-11-15).
//
// One run writes one timestamped folder under the configured root:
//
//	<root>/20261115T030000Z-nightly/
//	    db.dump            pg_dump -Fc of the kernel database
//	    files/config.db    the ADR-0101 config store (consistent bbolt copy)
//	    files/data/…       the storage data directory (agents.db journal, content store, …)
//	    manifest.json      written LAST: kind, database, schema version, exact per-table row
//	                       counts, and every file's size + sha256
//
// secret.key is NEVER written into that folder: it is encrypted with a passphrase
// (secretkey.go) and written to a separate root (<key root>/<same folder name>/secret.key.enc).
// The method is logical (pg_dump custom format). WAL-based point-in-time recovery
// (wal-g) is the documented production option, not built (cambrian-core/docs/backup-and-restore.md).
//
// Backups are deployment-owned: `orchestrator backup run|verify|list|prune|restore|drill`
// (app/backup_cli.go); scheduling is the host's (Task Scheduler, systemd timer, cron).
// The same Run takes the mandatory snapshot before a destructive migration
// (PreMigrationSnapshotter, internal/migrate destructive.go).
//
// AGENTS.md §2.2/§6: this package opens its own Postgres connections and runs pg_dump /
// pg_restore against the kernel database. It is an operator-run maintenance path, not a
// principal-serving one: it reads catalog metadata and row COUNTS only (never row
// content) through pgx, and moves data only through pg_dump / pg_restore, so it does not
// go through MemoryStack — there is no principal whose scope could apply.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kinds of run. Nightly and manual runs are rotated by Retention; snapshots taken before
// a destructive migration or a restore over the live database are kept until removed by hand.
const (
	KindNightly      = "nightly"
	KindManual       = "manual"
	KindPreMigration = "pre-migration"
	KindPreRestore   = "pre-restore"
)

// ManifestFormat is the manifest.json schema version.
const ManifestFormat = 1

// File kinds in a manifest.
const (
	FileDump  = "db-dump"
	FileBbolt = "bbolt"
	FilePlain = "file"
)

// File is one entry of a manifest; Name is slash-separated and relative to the run folder.
type File struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	// Note records how a bbolt file was copied ("consistent" or "hot-copy, checked").
	Note string `json:"note,omitempty"`
}

// TableCount is the exact row count of one table at the dump's snapshot.
type TableCount struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// SecretKeyRef says where this run's encrypted secret.key went — never the key itself.
type SecretKeyRef struct {
	Status string `json:"status"` // "encrypted", "skipped", "absent", "env"
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"` // of the ENCRYPTED file
	Note   string `json:"note,omitempty"`
}

// Manifest describes one backup folder.
type Manifest struct {
	Format        int           `json:"format"`
	Kind          string        `json:"kind"`
	Reason        string        `json:"reason,omitempty"`
	Created       time.Time     `json:"created"`
	Host          string        `json:"host"`
	Binary        string        `json:"binary,omitempty"`
	Database      string        `json:"database"`
	SchemaVersion int64         `json:"schema_version"`
	Tables        []TableCount  `json:"tables"`
	Files         []File        `json:"files"`
	SecretKey     *SecretKeyRef `json:"secret_key,omitempty"`
}

// Options configure one Run.
type Options struct {
	Root    string // backup root; the run writes <Root>/<stamp>-<kind>/
	KeyRoot string // root for the encrypted secret.key (never inside the run folder)
	Kind    string
	Reason  string
	PG      PG
	// ConfigStore is the config.db path ("" = not backed up, e.g. a pre-migration snapshot).
	ConfigStore string
	// DataDir is the storage data directory ("" = not backed up).
	DataDir string
	// SecretKeyPath is secret.key's path; "" = not part of this run.
	SecretKeyPath string
	// Passphrase encrypts secret.key; required when SecretKeyPath exists and SkipSecretKey is false.
	Passphrase    []byte
	PassphraseEnv string // only to name it in errors
	SkipSecretKey bool
	// Exclude are absolute paths never copied from DataDir (the backup roots, when nested).
	Exclude []string
	Binary  string
	Now     func() time.Time
	Log     func(format string, args ...any)
}

var runNameRE = regexp.MustCompile(`^(\d{8}T\d{6}Z)-([a-z][a-z-]*)$`)

// RunName is the folder name of a run created at t (UTC) of the given kind.
func RunName(t time.Time, kind string) string {
	return t.UTC().Format("20060102T150405Z") + "-" + kind
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Run writes one backup run and returns its folder and manifest. The manifest is written
// last (atomically), so a folder without one is an incomplete run that Verify refuses.
func Run(ctx context.Context, o Options) (string, *Manifest, error) {
	if o.Root == "" {
		return "", nil, errors.New("backup: no backup directory configured (backup.dir)")
	}
	switch o.Kind {
	case KindNightly, KindManual, KindPreMigration, KindPreRestore:
	default:
		return "", nil, fmt.Errorf("backup: unknown kind %q (want nightly | manual | pre-migration | pre-restore)", o.Kind)
	}
	// Refuse BEFORE any work when secret.key would be skipped silently.
	secretPresent := false
	if o.SecretKeyPath != "" && !o.SkipSecretKey {
		if _, err := os.Stat(o.SecretKeyPath); err == nil {
			secretPresent = true
			if len(o.Passphrase) == 0 {
				return "", nil, fmt.Errorf("backup: %s exists but no passphrase is set to encrypt it: set the "+
					"environment variable %s (backup.passphrase_env), or pass --no-secret-key to back up "+
					"everything else (secret.key is never written unencrypted)", o.SecretKeyPath, o.PassphraseEnv)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("backup: stat %s: %w", o.SecretKeyPath, err)
		}
	}
	if o.KeyRoot != "" && within(o.KeyRoot, o.Root) {
		return "", nil, fmt.Errorf("backup: the secret-key directory %s is inside the backup directory %s; "+
			"backup.key_dir must be kept apart from the dumps", o.KeyRoot, o.Root)
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	t := now().UTC()
	name := RunName(t, o.Kind)
	if err := os.MkdirAll(o.Root, 0o700); err != nil {
		return "", nil, fmt.Errorf("backup: create %s: %w", o.Root, err)
	}
	dir := filepath.Join(o.Root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("backup: create run folder: %w", err)
	}
	host, _ := os.Hostname()
	m := &Manifest{Format: ManifestFormat, Kind: o.Kind, Reason: o.Reason, Created: t, Host: host, Binary: o.Binary}

	// 1. Database: counts and dump at ONE snapshot.
	dumpPath := filepath.Join(dir, "db.dump")
	info, err := DumpDatabase(ctx, o.PG, dumpPath)
	if err != nil {
		return dir, nil, fmt.Errorf("backup: database: %w", err)
	}
	m.Database, m.SchemaVersion, m.Tables = info.Database, info.SchemaVersion, info.Tables
	f, err := describe(dir, "db.dump", FileDump)
	if err != nil {
		return dir, nil, err
	}
	m.Files = append(m.Files, *f)
	o.logf("database %s: %d bytes, %d tables, schema version %d", info.Database, f.Bytes, len(info.Tables), info.SchemaVersion)

	// 2. Config store.
	if o.ConfigStore != "" {
		if _, err := os.Stat(o.ConfigStore); err == nil {
			note, err := copyBbolt(o.ConfigStore, filepath.Join(dir, "files", "config.db"))
			if err != nil {
				return dir, nil, fmt.Errorf("backup: config store: %w", err)
			}
			f, err := describe(dir, "files/config.db", FileBbolt)
			if err != nil {
				return dir, nil, err
			}
			f.Note = note
			m.Files = append(m.Files, *f)
			o.logf("config store: %s (%s)", o.ConfigStore, note)
		} else if !errors.Is(err, os.ErrNotExist) {
			return dir, nil, fmt.Errorf("backup: stat config store: %w", err)
		}
	}

	// 3. Data directory.
	if o.DataDir != "" {
		if _, err := os.Stat(o.DataDir); err == nil {
			files, err := copyDataDir(o.DataDir, dir, "files/data", o.Exclude)
			if err != nil {
				return dir, nil, fmt.Errorf("backup: data directory: %w", err)
			}
			m.Files = append(m.Files, files...)
			o.logf("data directory: %s (%d files)", o.DataDir, len(files))
		} else if !errors.Is(err, os.ErrNotExist) {
			return dir, nil, fmt.Errorf("backup: stat data directory: %w", err)
		}
	}

	// 4. secret.key — encrypted, in the separate key root.
	switch {
	case o.SecretKeyPath == "":
	case o.SkipSecretKey:
		m.SecretKey = &SecretKeyRef{Status: "skipped", Note: "--no-secret-key: secret.key was not backed up by this run"}
	case !secretPresent:
		ref := &SecretKeyRef{Status: "absent", Note: o.SecretKeyPath + " does not exist"}
		if os.Getenv("CAMBRIAN_SECRET_KEY") != "" {
			ref.Status = "env"
			ref.Note = "the key is supplied by CAMBRIAN_SECRET_KEY; keep it in your secret manager"
		}
		m.SecretKey = ref
	default:
		if o.KeyRoot == "" {
			return dir, nil, errors.New("backup: no secret-key directory configured (backup.key_dir)")
		}
		out := filepath.Join(o.KeyRoot, name, SecretKeyFileName)
		if err := EncryptFile(o.SecretKeyPath, out, o.Passphrase); err != nil {
			return dir, nil, fmt.Errorf("backup: secret.key: %w", err)
		}
		sum, _, err := hashFile(out)
		if err != nil {
			return dir, nil, err
		}
		m.SecretKey = &SecretKeyRef{Status: "encrypted", Path: out, SHA256: sum}
		o.logf("secret.key: encrypted to %s", out)
	}

	if err := writeManifest(dir, m); err != nil {
		return dir, nil, err
	}
	return dir, m, nil
}

// within reports whether p is root or below it.
func within(p, root string) bool {
	ap, err1 := filepath.Abs(p)
	ar, err2 := filepath.Abs(root)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(ar, ap)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeManifest(dir string, m *Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encode manifest: %w", err)
	}
	tmp := filepath.Join(dir, "manifest.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("backup: write manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "manifest.json")); err != nil {
		return fmt.Errorf("backup: write manifest: %w", err)
	}
	return nil
}

// ReadManifest loads a run folder's manifest without checking the files.
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("%s is not a complete backup run (no manifest.json): %w", dir, err)
	}
	m := &Manifest{}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: manifest: %w", dir, err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("%s: manifest format %d, this binary reads %d", dir, m.Format, ManifestFormat)
	}
	return m, nil
}

// Verify checks a run folder: every manifest file exists with its size and sha256, every
// bbolt copy opens and passes bbolt's consistency check, the encrypted secret.key (when
// recorded) matches its sha256, and — when pg is non-nil — pg_restore can list the dump.
func Verify(ctx context.Context, dir string, pg *PG) (*Manifest, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	if len(m.Files) == 0 || m.Files[0].Kind != FileDump {
		return nil, fmt.Errorf("%s: manifest records no database dump", dir)
	}
	for _, f := range m.Files {
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		sum, n, err := hashFile(p)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", f.Name, err)
		}
		if sum != f.SHA256 || n != f.Bytes {
			return nil, fmt.Errorf("verify %s: does not match the manifest (%d bytes sha256 %s, want %d bytes %s)",
				f.Name, n, sum, f.Bytes, f.SHA256)
		}
		if f.Kind == FileBbolt {
			if err := checkBbolt(p); err != nil {
				return nil, fmt.Errorf("verify %s: %w", f.Name, err)
			}
		}
	}
	if k := m.SecretKey; k != nil && k.Status == "encrypted" {
		sum, _, err := hashFile(k.Path)
		if err != nil {
			return nil, fmt.Errorf("verify encrypted secret.key: %w", err)
		}
		if sum != k.SHA256 {
			return nil, fmt.Errorf("verify encrypted secret.key %s: sha256 mismatch", k.Path)
		}
	}
	if pg != nil {
		if err := pg.ListArchive(ctx, filepath.Join(dir, "db.dump")); err != nil {
			return nil, fmt.Errorf("verify db.dump: %w", err)
		}
	}
	return m, nil
}

// RunInfo is one run folder found under a root.
type RunInfo struct {
	Dir      string
	Name     string
	Kind     string
	Created  time.Time
	Complete bool
}

// ListRuns returns the run folders under root, newest first. Folders whose name is not a
// run name are ignored (never touched by Prune).
func ListRuns(root string) ([]RunInfo, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RunInfo
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		mm := runNameRE.FindStringSubmatch(e.Name())
		if mm == nil {
			continue
		}
		t, err := time.Parse("20060102T150405Z", mm[1])
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		_, statErr := os.Stat(filepath.Join(dir, "manifest.json"))
		out = append(out, RunInfo{Dir: dir, Name: e.Name(), Kind: mm[2], Created: t, Complete: statErr == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Latest returns the newest complete run of any kind, or of kind when non-empty.
func Latest(root, kind string) (string, error) {
	runs, err := ListRuns(root)
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		if r.Complete && (kind == "" || r.Kind == kind) {
			return r.Dir, nil
		}
	}
	return "", fmt.Errorf("no complete backup run under %s", root)
}

func describe(dir, rel, kind string) (*File, error) {
	sum, n, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	return &File{Name: rel, Kind: kind, Bytes: n, SHA256: sum}, nil
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
