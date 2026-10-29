// Package archive keeps source archives on disk, keyed by repository and commit, within a total
// size limit. The least recently used archives are deleted first.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	fileSuffix = ".tar.gz"
	tempSuffix = ".tmp"
)

// ErrTooLarge is returned by Store when the archive exceeds the size limit of one archive.
var ErrTooLarge = errors.New("archive: larger than the allowed archive size")

// Cache is a directory of archives. It is safe for concurrent use.
type Cache struct {
	dir      string
	maxTotal int64
	now      func() time.Time

	// mu serializes the changes of the directory: publishing a file, touching it and evicting.
	mu sync.Mutex
}

// Open prepares dir, which is created when missing, and removes downloads that a crash left
// unfinished. maxTotal is the total size limit of the stored archives. A nil now defaults to
// time.Now.
func Open(dir string, maxTotal int64, now func() time.Time) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("archive: create %s: %w", dir, err)
	}
	if now == nil {
		now = time.Now
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("archive: read %s: %w", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), tempSuffix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return &Cache{dir: dir, maxTotal: maxTotal, now: now}, nil
}

// Key identifies an archive by provider, host, repository path and commit.
func Key(provider, baseURL, fullPath, sha string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{provider, baseURL, fullPath, sha}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// Get opens the archive stored under key and marks it as used. ok is false when there is none.
// The caller closes the file; deleting an archive that is still open does not affect the reader.
func (c *Cache) Get(key string) (file *os.File, size int64, ok bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := c.path(key)
	file, err = os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("archive: open: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, false, fmt.Errorf("archive: stat: %w", err)
	}
	if err := os.Chtimes(path, c.now(), c.now()); err != nil {
		_ = file.Close()
		return nil, 0, false, fmt.Errorf("archive: mark used: %w", err)
	}
	return file, info.Size(), true, nil
}

// Put reads r into the cache under key and returns the stored size. A body longer than limit
// bytes is dropped and reported as ErrTooLarge. Afterwards the least recently used archives
// are deleted until the total size is within the cache limit; the new archive is kept.
func (c *Cache) Put(key string, r io.Reader, limit int64) (int64, error) {
	tmp, err := os.CreateTemp(c.dir, "download-*"+tempSuffix)
	if err != nil {
		return 0, fmt.Errorf("archive: create temp file: %w", err)
	}
	size, err := io.Copy(tmp, io.LimitReader(r, limit+1))
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil && size > limit {
		err = ErrTooLarge
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, wrapCopy(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.Rename(tmp.Name(), c.path(key)); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, fmt.Errorf("archive: publish: %w", err)
	}
	if err := os.Chtimes(c.path(key), c.now(), c.now()); err != nil {
		return 0, fmt.Errorf("archive: mark used: %w", err)
	}
	c.evict(key)
	return size, nil
}

func wrapCopy(err error) error {
	if errors.Is(err, ErrTooLarge) {
		return err
	}
	return fmt.Errorf("archive: store: %w", err)
}

// evict deletes the least recently used archives, except keep, until the total fits.
// Failures to delete are ignored: the file then stays and counts again next time.
func (c *Cache) evict(keep string) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type stored struct {
		path string
		size int64
		used time.Time
	}
	var files []stored
	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		if e.Name() != keep+fileSuffix {
			files = append(files, stored{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= c.maxTotal {
			return
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

func (c *Cache) path(key string) string { return filepath.Join(c.dir, key+fileSuffix) }
