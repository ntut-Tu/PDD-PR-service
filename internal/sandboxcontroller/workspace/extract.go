package workspace

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Extract unpacks a tar.gz repository archive into the checkout of runID. Both providers wrap
// the repository in one top-level directory, which is removed. Entries that would leave the
// checkout, symbolic links that point out of it, hard links and special files are rejected or
// skipped, and the unpacked size and file count are capped.
func (m *Manager) Extract(runID string, archive io.Reader) error {
	src := m.SrcDir(runID)
	gz, err := gzip.NewReader(io.LimitReader(archive, m.MaxBytes))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrArchive, err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	var (
		top       string
		remaining = m.MaxBytes
		files     int
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrArchive, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		rel, entryTop, err := strip(hdr.Name)
		if err != nil {
			return err
		}
		if top == "" {
			top = entryTop
		} else if entryTop != top {
			return fmt.Errorf("%w: more than one top-level entry (%q and %q)", ErrArchive, top, entryTop)
		}
		if rel == "" {
			continue
		}
		if files++; files > m.MaxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrArchive, m.MaxFiles)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = ensureDir(src, rel)
		case tar.TypeReg:
			var n int64
			n, err = extractFile(src, rel, tr, hdr, remaining)
			remaining -= n
		case tar.TypeSymlink:
			err = extractSymlink(src, rel, hdr.Linkname)
		case tar.TypeLink:
			err = fmt.Errorf("%w: hard link %q is not allowed", ErrArchive, rel)
		}
		if err != nil {
			return err
		}
	}
	if top == "" {
		return fmt.Errorf("%w: archive is empty", ErrArchive)
	}
	return nil
}

// strip returns the entry path without its top-level directory, and that directory. An empty
// result is the top-level directory itself.
func strip(name string) (rel, top string, err error) {
	if strings.ContainsRune(name, 0) || strings.HasPrefix(name, "/") {
		return "", "", fmt.Errorf("%w: unsafe entry name %q", ErrArchive, name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", "", fmt.Errorf("%w: path traversal in entry %q", ErrArchive, name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", "", fmt.Errorf("%w: unexpected entry %q", ErrArchive, name)
	}
	top, rel, _ = strings.Cut(clean, "/")
	return rel, top, nil
}

func extractFile(src, rel string, r io.Reader, hdr *tar.Header, remaining int64) (int64, error) {
	if hdr.Size > remaining {
		return 0, fmt.Errorf("%w: unpacked size exceeds the limit", ErrArchive)
	}
	mode := os.FileMode(0o666)
	if hdr.Mode&0o111 != 0 {
		mode = 0o777
	}
	f, err := createFile(src, rel, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, remaining+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("%w: %v", ErrArchive, err)
	}
	if n > remaining {
		return n, fmt.Errorf("%w: unpacked size exceeds the limit", ErrArchive)
	}
	return n, nil
}

func extractSymlink(src, rel, target string) error {
	if target == "" || path.IsAbs(target) || strings.ContainsRune(target, 0) {
		return fmt.Errorf("%w: symbolic link %q points to %q", ErrArchive, rel, target)
	}
	resolved := path.Join(path.Dir(rel), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("%w: symbolic link %q escapes the workspace", ErrArchive, rel)
	}
	if i := strings.LastIndex(rel, "/"); i > 0 {
		if err := ensureDir(src, rel[:i]); err != nil {
			return err
		}
	}
	if err := os.Symlink(target, filepath.Join(src, filepath.FromSlash(rel))); err != nil {
		return fmt.Errorf("workspace: symlink %s: %w", rel, err)
	}
	return nil
}
