// Package workspace prepares the directories a sandbox sees: the unpacked repository at the
// requested commit with the generated test files written into it, the init files of the build,
// and the per-repository dependency cache.
//
// Everything that comes from outside (the archive entries, the generated file paths) is treated
// as hostile: names are checked for traversal, no path is created through a symbolic link, and
// the unpacked size is capped.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"pdd/services/internal/sandboxcontroller/model"
)

// ErrArchive wraps every problem with the content of the source archive, as opposed to a local
// file system failure.
var ErrArchive = errors.New("invalid source archive")

// Directory names inside one run directory.
const (
	srcName  = "src"
	initName = ".pdd"
)

// Manager owns the workspace root directory.
type Manager struct {
	Root     string
	MaxBytes int64
	MaxFiles int
}

// Dir is the directory of one run; it is what the sandbox mounts.
func (m *Manager) Dir(runID string) string { return filepath.Join(m.Root, runID) }

// SrcDir is the repository checkout of a run.
func (m *Manager) SrcDir(runID string) string { return filepath.Join(m.Dir(runID), srcName) }

// InitDir holds the build configuration written by the controller.
func (m *Manager) InitDir(runID string) string { return filepath.Join(m.Dir(runID), initName) }

// Init creates the empty directories of a run, replacing leftovers of an earlier attempt.
func (m *Manager) Init(runID string) error {
	if err := m.Remove(runID); err != nil {
		return err
	}
	if err := os.MkdirAll(m.Root, 0o750); err != nil {
		return fmt.Errorf("workspace: create root: %w", err)
	}
	for _, dir := range []string{m.Dir(runID), m.SrcDir(runID), m.InitDir(runID)} {
		if err := mkdirOpen(dir); err != nil {
			return err
		}
	}
	return nil
}

// Remove deletes the directory of a run; a missing directory is not an error.
func (m *Manager) Remove(runID string) error {
	if err := os.RemoveAll(m.Dir(runID)); err != nil {
		return fmt.Errorf("workspace: remove %s: %w", runID, err)
	}
	return nil
}

// List returns the names of the run directories under the root, sorted.
func (m *Manager) List() ([]string, error) {
	entries, err := os.ReadDir(m.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: list: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// WriteFiles writes the generated test files into the checkout. Every path must lie under a
// src/test directory.
func (m *Manager) WriteFiles(runID string, files []model.File) error {
	src := m.SrcDir(runID)
	for _, f := range files {
		rel, err := model.CleanTestPath(f.Path)
		if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		if err := writeFile(src, rel, []byte(f.Content), 0o666); err != nil {
			return err
		}
	}
	return nil
}

// CacheDir creates and returns the dependency cache directory of a repository below root.
// The project and repository ids come from the run spec, so the path cannot be influenced
// by repository content.
func CacheDir(root string, projectID, repoID int64) (string, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("workspace: create cache root: %w", err)
	}
	dir := filepath.Join(root, strconv.FormatInt(projectID, 10), strconv.FormatInt(repoID, 10))
	if err := mkdirAllOpen(root, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// mkdirOpen creates a directory that the unprivileged build user may write to. The run
// directory itself is protected by the 0750 root, so the open mode is only visible inside.
func mkdirOpen(dir string) error {
	if err := os.Mkdir(dir, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("workspace: mkdir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return fmt.Errorf("workspace: chmod %s: %w", dir, err)
	}
	return nil
}

func mkdirAllOpen(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	cur := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, seg)
		if err := mkdirOpen(cur); err != nil {
			return err
		}
	}
	return nil
}

// ensureDir creates the directories of rel (a slash separated path below root) one by one and
// fails when an existing component is a symbolic link or not a directory.
func ensureDir(root, rel string) error {
	cur := root
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := mkdirOpen(cur); err != nil {
				return err
			}
		case err != nil:
			return fmt.Errorf("workspace: stat %s: %w", cur, err)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%w: %q goes through a symbolic link", ErrArchive, rel)
		case !info.IsDir():
			return fmt.Errorf("%w: %q has a file where a directory is expected", ErrArchive, rel)
		}
	}
	return nil
}

// writeFile creates rel below root with data and never follows a symbolic link, neither in
// the directories nor as the last component.
func writeFile(root, rel string, data []byte, mode os.FileMode) error {
	f, err := createFile(root, rel, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("workspace: write %s: %w", rel, err)
	}
	return f.Close()
}

func createFile(root, rel string, mode os.FileMode) (*os.File, error) {
	if i := strings.LastIndex(rel, "/"); i > 0 {
		if err := ensureDir(root, rel[:i]); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: %q is a symbolic link", ErrArchive, rel)
		}
		return nil, fmt.Errorf("workspace: create %s: %w", rel, err)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workspace: chmod %s: %w", rel, err)
	}
	return f, nil
}
