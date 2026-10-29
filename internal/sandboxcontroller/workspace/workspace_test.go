package workspace_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/workspace"
)

type entry struct {
	name string
	body string
	typ  byte
	link string
	mode int64
}

func archive(t *testing.T, entries ...entry) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeXGlobalHeader {
			hdr = &tar.Header{Name: e.name, Typeflag: typ, PAXRecords: map[string]string{"comment": "0123"}}
		}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return bytes.NewReader(buf.Bytes())
}

func newManager(t *testing.T) (*workspace.Manager, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	m := &workspace.Manager{Root: root, MaxBytes: 1 << 20, MaxFiles: 100}
	if err := m.Init("run_1"); err != nil {
		t.Fatal(err)
	}
	return m, root
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExtractStripsTopLevelDirectory(t *testing.T) {
	m, _ := newManager(t)
	a := archive(t,
		entry{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		entry{name: "octo-hello-abc123/", typ: tar.TypeDir},
		entry{name: "octo-hello-abc123/pom.xml", body: "<project/>"},
		entry{name: "octo-hello-abc123/src/main/java/A.java", body: "class A {}"},
		entry{name: "octo-hello-abc123/mvnw", body: "#!/bin/sh", mode: 0o755},
	)

	if err := m.Extract("run_1", a); err != nil {
		t.Fatal(err)
	}

	src := m.SrcDir("run_1")
	if read(t, filepath.Join(src, "pom.xml")) != "<project/>" || read(t, filepath.Join(src, "src/main/java/A.java")) != "class A {}" {
		t.Error("files not unpacked")
	}
	if info, _ := os.Stat(filepath.Join(src, "mvnw")); info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mvnw lost its executable bit: %v", info.Mode())
	}
	if info, _ := os.Stat(filepath.Join(src, "pom.xml")); info.Mode().Perm() != 0o666 {
		t.Errorf("pom.xml mode = %v", info.Mode())
	}
}

func TestExtractAcceptsDotSlashPrefixAndSafeSymlinks(t *testing.T) {
	m, _ := newManager(t)
	a := archive(t,
		entry{name: "./top/", typ: tar.TypeDir},
		entry{name: "./top/lib/real.txt", body: "x"},
		entry{name: "./top/link.txt", typ: tar.TypeSymlink, link: "lib/real.txt"},
		entry{name: "./top/lib/up.txt", typ: tar.TypeSymlink, link: "../link.txt"},
		entry{name: "./top/dev", typ: tar.TypeChar},
	)

	if err := m.Extract("run_1", a); err != nil {
		t.Fatal(err)
	}

	if got := read(t, filepath.Join(m.SrcDir("run_1"), "link.txt")); got != "x" {
		t.Errorf("link content = %q", got)
	}
}

func TestExtractRejectsUnsafeArchives(t *testing.T) {
	cases := map[string][]entry{
		"traversal":            {{name: "top/../../evil", body: "x"}},
		"traversal at start":   {{name: "../evil", body: "x"}},
		"absolute":             {{name: "/etc/passwd", body: "x"}},
		"two top levels":       {{name: "a/x", body: "x"}, {name: "b/y", body: "y"}},
		"absolute symlink":     {{name: "top/l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"escaping symlink":     {{name: "top/l", typ: tar.TypeSymlink, link: "../../x"}},
		"escaping nested link": {{name: "top/a/b/l", typ: tar.TypeSymlink, link: "../../../x"}},
		"empty symlink":        {{name: "top/l", typ: tar.TypeSymlink, link: ""}},
		"hard link":            {{name: "top/a", body: "x"}, {name: "top/b", typ: tar.TypeLink, link: "top/a"}},
		"write through link":   {{name: "top/d", typ: tar.TypeSymlink, link: "."}, {name: "top/d/x", body: "x"}},
		"overwrite link":       {{name: "top/l", typ: tar.TypeSymlink, link: "a"}, {name: "top/l", body: "x"}},
		"file as directory":    {{name: "top/f", body: "x"}, {name: "top/f/g", body: "y"}},
		"dot entry":            {{name: ".", typ: tar.TypeDir}},
		"empty":                {},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			m, root := newManager(t)

			err := m.Extract("run_1", archive(t, entries...))

			if !errors.Is(err, workspace.ErrArchive) {
				t.Fatalf("err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil")); err == nil {
				t.Error("file written outside the workspace")
			}
		})
	}
}

func TestExtractEnforcesLimits(t *testing.T) {
	t.Run("size by header", func(t *testing.T) {
		m, _ := newManager(t)
		m.MaxBytes = 100
		err := m.Extract("run_1", archive(t, entry{name: "t/a", body: strings.Repeat("x", 200)}))
		if !errors.Is(err, workspace.ErrArchive) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("size across files", func(t *testing.T) {
		m, _ := newManager(t)
		m.MaxBytes = 100
		err := m.Extract("run_1", archive(t, entry{name: "t/a", body: strings.Repeat("x", 60)}, entry{name: "t/b", body: strings.Repeat("y", 60)}))
		if !errors.Is(err, workspace.ErrArchive) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("file count", func(t *testing.T) {
		m, _ := newManager(t)
		m.MaxFiles = 2
		err := m.Extract("run_1", archive(t, entry{name: "t/a"}, entry{name: "t/b"}, entry{name: "t/c"}))
		if !errors.Is(err, workspace.ErrArchive) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("not gzip", func(t *testing.T) {
		m, _ := newManager(t)
		if err := m.Extract("run_1", strings.NewReader("hello")); !errors.Is(err, workspace.ErrArchive) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		m, _ := newManager(t)
		data := archive(t, entry{name: "t/a", body: strings.Repeat("x", 5000)})
		raw := make([]byte, data.Len())
		_, _ = data.Read(raw)
		if err := m.Extract("run_1", bytes.NewReader(raw[:len(raw)/2])); !errors.Is(err, workspace.ErrArchive) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestWriteFilesPlacesTestsUnderSrcTest(t *testing.T) {
	m, _ := newManager(t)
	files := []model.File{
		{Path: "src/test/java/a/BTest.java", Content: "class BTest {}"},
		{Path: "module/src/test/java/c/CTest.java", Content: "class CTest {}"},
	}

	if err := m.WriteFiles("run_1", files); err != nil {
		t.Fatal(err)
	}

	if read(t, filepath.Join(m.SrcDir("run_1"), "src/test/java/a/BTest.java")) != "class BTest {}" {
		t.Error("BTest missing")
	}
	if read(t, filepath.Join(m.SrcDir("run_1"), "module/src/test/java/c/CTest.java")) != "class CTest {}" {
		t.Error("CTest missing")
	}
}

func TestWriteFilesReplacesExistingTest(t *testing.T) {
	m, _ := newManager(t)
	_ = m.Extract("run_1", archive(t, entry{name: "t/src/test/A.java", body: "old"}))

	if err := m.WriteFiles("run_1", []model.File{{Path: "src/test/A.java", Content: "new"}}); err != nil {
		t.Fatal(err)
	}

	if read(t, filepath.Join(m.SrcDir("run_1"), "src/test/A.java")) != "new" {
		t.Error("file not replaced")
	}
}

func TestWriteFilesRejectsEscapes(t *testing.T) {
	m, _ := newManager(t)
	outside := t.TempDir()
	// The repository itself contains src/test as a symlink to a directory outside.
	if err := os.Symlink(outside, filepath.Join(m.SrcDir("run_1"), "src")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(m.SrcDir("run_1"), "m/src/test"), 0o755)
	if err := os.Symlink(filepath.Join(outside, "target.java"), filepath.Join(m.SrcDir("run_1"), "m/src/test/L.java")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"src/test/A.java", "m/src/test/L.java", "../x/src/test/A.java", "pom.xml", "/src/test/A.java"} {
		err := m.WriteFiles("run_1", []model.File{{Path: path, Content: "boom"}})
		if err == nil {
			t.Errorf("%s accepted", path)
		}
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Errorf("wrote outside: %v", entries)
	}
}

func TestInitRemoveAndList(t *testing.T) {
	m, root := newManager(t)
	if err := m.Init("run_2"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "stray-file"), nil, 0o600)

	names, err := m.List()
	if err != nil || len(names) != 2 || names[0] != "run_1" || names[1] != "run_2" {
		t.Fatalf("names = %v, %v", names, err)
	}
	for _, dir := range []string{m.Dir("run_1"), m.SrcDir("run_1"), m.InitDir("run_1")} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o777 {
			t.Errorf("%s: %v %v", dir, info, err)
		}
	}
	if info, _ := os.Stat(root); info.Mode().Perm() != 0o750 {
		t.Errorf("root mode = %v", info.Mode())
	}

	_ = os.WriteFile(filepath.Join(m.SrcDir("run_1"), "left"), nil, 0o600)
	if err := m.Init("run_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.SrcDir("run_1"), "left")); err == nil {
		t.Error("Init kept leftovers")
	}

	if err := m.Remove("run_1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("run_1"); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	if names, _ := m.List(); len(names) != 1 {
		t.Errorf("names = %v", names)
	}
}

func TestListOfMissingRootIsEmpty(t *testing.T) {
	m := &workspace.Manager{Root: filepath.Join(t.TempDir(), "none")}
	if names, err := m.List(); err != nil || len(names) != 0 {
		t.Errorf("names = %v, err = %v", names, err)
	}
}

func TestCacheDirIsPerRepository(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")

	a, err := workspace.CacheDir(root, 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := workspace.CacheDir(root, 7, 2)
	again, _ := workspace.CacheDir(root, 7, 1)

	if a != filepath.Join(root, "7", "1") || a == b || a != again {
		t.Errorf("dirs = %s %s %s", a, b, again)
	}
	if info, err := os.Stat(a); err != nil || info.Mode().Perm() != 0o777 {
		t.Errorf("mode = %v, %v", info, err)
	}
}
