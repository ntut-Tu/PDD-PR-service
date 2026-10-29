package archive_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/archive"
)

// clock advances one second per reading, so the order of use is the order of the calls.
type clock struct{ t time.Time }

func (c *clock) now() time.Time {
	c.t = c.t.Add(time.Second)
	return c.t
}

func open(t *testing.T, maxTotal int64) (*archive.Cache, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "archives")
	c, err := archive.Open(dir, maxTotal, (&clock{t: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}).now)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func put(t *testing.T, c *archive.Cache, key, content string) {
	t.Helper()
	if _, err := c.Put(key, strings.NewReader(content), 1000); err != nil {
		t.Fatalf("Put %s: %v", key, err)
	}
}

func has(t *testing.T, c *archive.Cache, key string) bool {
	t.Helper()
	f, _, ok, err := c.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		_ = f.Close()
	}
	return ok
}

func TestPutAndGet(t *testing.T) {
	c, _ := open(t, 100)

	size, err := c.Put("k", strings.NewReader("hello"), 10)
	if err != nil || size != 5 {
		t.Fatalf("Put = %d, %v", size, err)
	}
	f, got, ok, err := c.Get("k")
	if err != nil || !ok || got != 5 {
		t.Fatalf("Get = %d, %v, %v", got, ok, err)
	}
	defer func() { _ = f.Close() }()
	if body, _ := io.ReadAll(f); string(body) != "hello" {
		t.Errorf("body = %q", body)
	}
	if _, _, ok, _ := c.Get("missing"); ok {
		t.Error("missing key found")
	}
}

func TestPutRejectsTooLargeBodyAndLeavesNothingBehind(t *testing.T) {
	c, dir := open(t, 100)

	_, err := c.Put("k", strings.NewReader("12345678901"), 10)

	if !errors.Is(err, archive.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if has(t, c, "k") {
		t.Error("oversized archive was stored")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
	if _, err := c.Put("k", strings.NewReader("1234567890"), 10); err != nil {
		t.Errorf("body at the limit rejected: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestPutFailedDownloadLeavesNothingBehind(t *testing.T) {
	c, dir := open(t, 100)

	if _, err := c.Put("k", failingReader{}, 10); err == nil || errors.Is(err, archive.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestEvictsLeastRecentlyUsedFirst(t *testing.T) {
	c, _ := open(t, 10)
	put(t, c, "a", "aaaa")
	put(t, c, "b", "bbbb")
	if !has(t, c, "a") { // a becomes more recent than b
		t.Fatal("a missing")
	}

	put(t, c, "c", "cccc")

	if has(t, c, "b") {
		t.Error("b, the least recently used, was kept")
	}
	if !has(t, c, "a") || !has(t, c, "c") {
		t.Error("a recently used archive was evicted")
	}
}

func TestEvictsSeveralAndKeepsTheNewArchive(t *testing.T) {
	c, _ := open(t, 10)
	put(t, c, "a", "aaaa")
	put(t, c, "b", "bbbb")

	put(t, c, "c", "cccccccccc")

	if has(t, c, "a") || has(t, c, "b") || !has(t, c, "c") {
		t.Error("expected only c to remain")
	}
}

func TestOpenRemovesUnfinishedDownloads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "archives")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "download-1.tmp")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := archive.Open(dir, 10, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale download kept: %v", err)
	}
}

func TestOpenFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Open(filepath.Join(file, "sub"), 10, nil); err == nil {
		t.Error("Open under a file succeeded")
	}
}

func TestKeyDependsOnEveryPart(t *testing.T) {
	base := archive.Key("github", "", "o/n", "abc")
	for name, other := range map[string]string{
		"provider": archive.Key("gitlab", "", "o/n", "abc"),
		"host":     archive.Key("github", "https://h", "o/n", "abc"),
		"path":     archive.Key("github", "", "o/m", "abc"),
		"sha":      archive.Key("github", "", "o/n", "abd"),
	} {
		if other == base {
			t.Errorf("key ignores the %s", name)
		}
	}
	if archive.Key("github", "", "o/n", "abc") != base {
		t.Error("key is not stable")
	}
}

func TestEvictedArchiveStaysReadableWhileOpen(t *testing.T) {
	c, _ := open(t, 4)
	put(t, c, "a", "aaaa")
	f, _, ok, _ := c.Get("a")
	if !ok {
		t.Fatal("a missing")
	}
	defer func() { _ = f.Close() }()

	put(t, c, "b", "bbbb")

	if body, _ := io.ReadAll(f); string(body) != "aaaa" {
		t.Errorf("open archive changed: %q", body)
	}
}

func TestPutReportsAnUnwritableTarget(t *testing.T) {
	c, dir := open(t, 100)
	blocker := filepath.Join(dir, "k.tar.gz")
	if err := os.MkdirAll(filepath.Join(blocker, "inner"), 0o750); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Put("k", strings.NewReader("x"), 10); err == nil {
		t.Fatal("Put over a directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}
