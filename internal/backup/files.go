package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	bberrors "go.etcd.io/bbolt/errors"
)

// copyBbolt copies a bbolt file. When its lock is free it is read through a read-only
// transaction (tx.WriteTo — a consistent copy). When a running kernel holds it, the file
// is copied raw and the copy must open and pass bbolt's consistency check; a torn copy
// is retried, then refused. bbolt locks a byte far past the file's end on Windows and
// uses advisory flock elsewhere, so a raw read of a live file is possible on both.
func copyBbolt(src, dst string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	db, err := bbolt.Open(src, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err == nil {
		defer db.Close()
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		werr := db.View(func(tx *bbolt.Tx) error {
			_, err := tx.WriteTo(out)
			return err
		})
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", fmt.Errorf("copy %s: %w", src, werr)
		}
		return "consistent", nil
	}
	if !errors.Is(err, bberrors.ErrTimeout) {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		_ = os.Remove(dst)
		if err := copyFile(src, dst); err != nil {
			return "", err
		}
		if lastErr = checkBbolt(dst); lastErr == nil {
			return "hot copy (file locked by a running process), consistency-checked", nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("%s is held by a running process and three raw copies failed bbolt's check (%v); "+
		"stop the kernel (`orchestrator stop`) and run the backup again", src, lastErr)
}

// checkBbolt opens a bbolt copy read-only and runs its consistency check.
func checkBbolt(p string) error {
	db, err := bbolt.Open(p, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return fmt.Errorf("open bbolt copy: %w", err)
	}
	defer db.Close()
	return db.View(func(tx *bbolt.Tx) error {
		for err := range tx.Check() {
			if err != nil {
				return fmt.Errorf("bbolt check: %w", err)
			}
		}
		return nil
	})
}

// isBbolt reports whether a file starts like a bbolt database (meta page magic).
func isBbolt(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 32)
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	// page header is 16 bytes; meta.magic (uint32, little endian) follows.
	return buf[16] == 0xED && buf[17] == 0xDA && buf[18] == 0x0C && buf[19] == 0xED
}

// copyDataDir copies the storage data directory into <dir>/<prefix>, bbolt files
// consistently, everything else byte for byte, skipping exclude paths.
func copyDataDir(src, dir, prefix string, exclude []string) ([]File, error) {
	var out []File
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, x := range exclude {
			if x != "" && within(p, x) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // sockets, symlinks: not state
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		dst := filepath.Join(dir, filepath.FromSlash(name))
		kind, note := FilePlain, ""
		if isBbolt(p) {
			kind = FileBbolt
			if note, err = copyBbolt(p, dst); err != nil {
				return err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := copyFile(p, dst); err != nil {
				return err
			}
		}
		f, err := describe(dir, name, kind)
		if err != nil {
			return err
		}
		f.Note = note
		out = append(out, *f)
		return nil
	})
	return out, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return out.Close()
}

// ExtractFiles copies a verified run's files (config.db, data/) into target, which must
// not exist yet: a restore never overwrites the live config bundle or data directory in
// place — the operator stops the kernel and moves the extracted files.
func ExtractFiles(dir string, m *Manifest, target string) (int, error) {
	if _, err := os.Stat(target); err == nil {
		return 0, fmt.Errorf("%s already exists; extract into a new directory", target)
	}
	n := 0
	for _, f := range m.Files {
		if !strings.HasPrefix(f.Name, "files/") {
			continue
		}
		dst := filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(f.Name, "files/")))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return n, err
		}
		if err := copyFile(filepath.Join(dir, filepath.FromSlash(f.Name)), dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
