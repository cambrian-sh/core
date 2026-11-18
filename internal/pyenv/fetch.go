package pyenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultIndexURL is the package index when execution.agents.python.index_url
// is empty (D7.3).
const DefaultIndexURL = "https://pypi.org/simple"

// maxArchiveBytes bounds one interpreter/installer download (the largest pinned
// archive is ~70 MB).
const maxArchiveBytes = 1 << 30

// Sources is where environment work fetches from (D7.3,
// execution.agents.python). The pin table's digests apply whatever the source,
// so a mirror cannot substitute a different build.
type Sources struct {
	// PythonMirror / InstallerMirror: a base URL (https:// or file://) serving
	// the pinned asset names. "" ⇒ the pin table's upstream release URL.
	PythonMirror    string
	InstallerMirror string
	// IndexURL is the package index. "" ⇒ DefaultIndexURL.
	IndexURL string
	// Wheelhouse: absolute local directories (or file:// URLs) of wheels.
	Wheelhouse []string
	// Offline: no network for env work — archives come from <root>/downloads or
	// a file:// mirror, wheels only from Wheelhouse.
	Offline bool
}

// ErrOfflineMissing marks an offline build that lacks a file it needs.
var ErrOfflineMissing = errors.New("offline and not available locally")

// HashMismatchError is a downloaded (or stored) archive whose SHA-256 is not the
// pinned one. The file has been deleted; nothing is retried from another source
// (D7.2.2, failure table).
type HashMismatchError struct {
	File   string
	Source string
	Want   string
	Got    string
}

func (e *HashMismatchError) Error() string {
	return fmt.Sprintf("sha256 mismatch for %s (from %s): pinned %s, got %s; the file was deleted and the build stopped",
		e.File, e.Source, e.Want, e.Got)
}

// localPath reports whether src names a local path (file:// URL or an absolute
// path) and returns it.
func localPath(src string) (string, bool) {
	if strings.HasPrefix(src, "file://") {
		u, err := url.Parse(src)
		if err != nil {
			return "", false
		}
		p := u.Path
		if u.Host != "" && u.Host != "localhost" {
			p = "//" + u.Host + p // UNC share
		}
		// file:///C:/x parses to "/C:/x" on every OS.
		if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		return filepath.FromSlash(p), true
	}
	if filepath.IsAbs(src) {
		return src, true
	}
	return "", false
}

// fileSHA256 is the lower-case hex SHA-256 of a file.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ensureArchive returns <downloads>/<asset.Name>, verified against asset.SHA256.
// A stored archive is re-verified; a missing one is fetched from base (a mirror
// or the upstream URL; a file:// or absolute base is a local copy, allowed
// offline). Every byte is hashed before the file takes its final name, so
// downloads/ only ever holds verified archives.
func ensureArchive(ctx context.Context, client *http.Client, downloads string, asset Asset, base string, offline bool) (string, error) {
	dest := filepath.Join(downloads, asset.Name)
	if _, err := os.Stat(dest); err == nil {
		got, err := fileSHA256(dest)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", dest, err)
		}
		if got == asset.SHA256 {
			return dest, nil
		}
		_ = os.Remove(dest) // a tampered or corrupted archive must not be reused
		return "", &HashMismatchError{File: asset.Name, Source: dest, Want: asset.SHA256, Got: got}
	}
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		return "", err
	}

	var (
		body   io.ReadCloser
		source string
	)
	if dir, ok := localPath(base); ok {
		source = filepath.Join(dir, asset.Name)
		f, err := os.Open(source)
		if err != nil {
			return "", fmt.Errorf("%s: %w", source, err)
		}
		body = f
	} else {
		if offline {
			return "", fmt.Errorf("%s is not in %s and the mirror is not a local path: %w", asset.Name, downloads, ErrOfflineMissing)
		}
		source = strings.TrimRight(base, "/") + "/" + strings.ReplaceAll(url.PathEscape(asset.Name), "+", "%2B")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("download %s: %w", source, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("download %s: %s", source, resp.Status)
		}
		body = resp.Body
	}
	defer body.Close()

	tmp, err := os.CreateTemp(downloads, ".part-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, maxArchiveBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxArchiveBytes {
		err = fmt.Errorf("%s exceeds %d bytes", source, maxArchiveBytes)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("fetch %s: %w", source, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != asset.SHA256 {
		_ = os.Remove(tmp.Name())
		return "", &HashMismatchError{File: asset.Name, Source: source, Want: asset.SHA256, Got: got}
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return dest, nil
}
