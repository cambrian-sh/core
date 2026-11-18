package pyenv

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxExtractBytes bounds decompression of one archive (an extracted
// install_only build is ~150-250 MB).
const maxExtractBytes = 2 << 30

// ErrArchiveEscape marks an archive entry that would land outside the
// extraction directory (D7.2.2: entries that escape are refused).
var ErrArchiveEscape = errors.New("archive entry escapes the extraction directory")

// within joins rel under root and refuses anything that leaves it.
func within(root, rel string) (string, error) {
	clean := path.Clean("/" + rel)
	if clean == "/" {
		return root, nil
	}
	if strings.Contains(rel, "\\") || filepath.IsAbs(rel) || strings.Contains(rel, ":") {
		return "", fmt.Errorf("%w: %q", ErrArchiveEscape, rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q", ErrArchiveEscape, rel)
		}
	}
	return filepath.Join(root, filepath.FromSlash(clean[1:])), nil
}

// extractTarGz extracts a .tar.gz into dest, dropping the leading strip
// component ("python/" in an install_only build). Regular files, directories
// and symlinks whose target stays inside dest are written; any other entry
// type is skipped; an entry or link target that escapes dest fails the whole
// extraction.
func extractTarGz(archive, dest, strip string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if strip != "" {
			if name == strings.TrimSuffix(strip, "/") {
				continue
			}
			if !strings.HasPrefix(name, strip) {
				return fmt.Errorf("%w: %q is not under %q", ErrArchiveEscape, hdr.Name, strip)
			}
			name = strings.TrimPrefix(name, strip)
		}
		if name == "" {
			continue
		}
		target, err := within(dest, name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			total += hdr.Size
			if total > maxExtractBytes {
				return fmt.Errorf("%s: extracted size exceeds %d bytes", archive, maxExtractBytes)
			}
			if err := writeFile(target, tr, hdr.FileInfo().Mode().Perm()|0o600, hdr.Size); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The link target is resolved relative to the link's own directory
			// and must stay inside dest (Linux/macOS builds link bin/python3 →
			// python3.12 and similar).
			resolved := path.Clean(path.Join(path.Dir(name), hdr.Linkname))
			if filepath.IsAbs(hdr.Linkname) || strings.HasPrefix(hdr.Linkname, "/") ||
				strings.Contains(hdr.Linkname, "\\") || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("%w: symlink %q -> %q", ErrArchiveEscape, hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Hard links, devices, FIFOs: not part of an install_only build.
		}
	}
}

func writeFile(target string, r io.Reader, mode os.FileMode, size int64) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(r, size)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// extractOne pulls the single file whose base name is base out of a .zip or
// .tar.gz archive into dest (0755). dest is chosen by the caller, so no entry
// name ever becomes a path.
func extractOne(archive, base, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, f := range zr.File {
			if path.Base(f.Name) != base {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(dest, rc, 0o755, maxArchiveBytes)
		}
		return fmt.Errorf("%s not found in %s", base, archive)
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not found in %s", base, archive)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == base {
			return writeFile(dest, tr, 0o755, hdr.Size)
		}
	}
}
