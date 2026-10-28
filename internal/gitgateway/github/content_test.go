package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/github"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/provider/providertest"
)

func newRESTProvider(t *testing.T, srv *providertest.RESTServer, ttl time.Duration, filesMaxPages int) *github.Provider {
	t.Helper()
	return github.New(github.Config{
		APIURL: srv.URL + "/", Client: providertest.NewClient(t), DetailTTL: ttl, TreeTTL: ttl, FileTTL: ttl,
		FilesMaxPages: filesMaxPages, ExpectedCost: 1,
	})
}

func fileJSON(name, status string) string {
	return fmt.Sprintf(`{"filename":%q,"status":%q,"additions":3,"deletions":1,"patch":"@@ -1 +1 @@\n-a\n+b"}`, name, status)
}

func TestPullFilesMapsStatusesAndPatches(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) {
		providertest.JSON(w, `[`+fileJSON("src/A.java", "added")+`,`+fileJSON("B.java", "copied")+`,`+
			`{"filename":"new/C.java","previous_filename":"old/C.java","status":"renamed","additions":0,"deletions":0},`+
			fileJSON("D.java", "removed")+`,`+fileJSON("E.java", "changed")+`,`+fileJSON("F.java", "modified")+`]`)
	})
	p := newRESTProvider(t, srv, time.Minute, 5)

	res, meta, err := p.PullFiles(context.Background(), "tok", repo(1, "o", "n"), 42)

	if err != nil || res.Truncated || meta.FetchedAt.IsZero() {
		t.Fatalf("PullFiles = %+v %+v %v", res, meta, err)
	}
	reqs := srv.Requests()
	if len(reqs) != 1 || reqs[0].Path != "/repos/o/n/pulls/42/files" || reqs[0].Query != "per_page=100&page=1" || reqs[0].Authorization != "Bearer tok" {
		t.Fatalf("requests = %+v", reqs)
	}
	want := []string{"added", "added", "renamed", "removed", "modified", "modified"}
	for i, status := range want {
		if res.Files[i].Status != status {
			t.Errorf("file %d status = %q, want %q", i, res.Files[i].Status, status)
		}
	}
	first := res.Files[0]
	if first.Path != "src/A.java" || first.Additions != 3 || first.Deletions != 1 || first.Patch != "@@ -1 +1 @@\n-a\n+b" {
		t.Errorf("first = %+v", first)
	}
	if renamed := res.Files[2]; renamed.Path != "new/C.java" || renamed.PreviousPath != "old/C.java" || renamed.Patch != "" {
		t.Errorf("renamed = %+v", renamed)
	}
}

func fullPage(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fileJSON(fmt.Sprintf("f%d.java", i), "modified")
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestPullFilesFollowsPagesAndFlagsTruncation(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(req providertest.RESTRequest, w http.ResponseWriter) {
		if strings.HasSuffix(req.Query, "page=3") {
			providertest.JSON(w, `[`+fileJSON("last.java", "added")+`]`)
			return
		}
		providertest.JSON(w, fullPage(100))
	})

	res, _, err := newRESTProvider(t, srv, time.Minute, 5).PullFiles(context.Background(), "tok", repo(1, "o", "n"), 1)
	if err != nil || len(res.Files) != 201 || res.Truncated || len(srv.Requests()) != 3 {
		t.Fatalf("all pages: %d files, truncated %v, %d requests, err %v", len(res.Files), res.Truncated, len(srv.Requests()), err)
	}

	capped, _, err := newRESTProvider(t, srv, time.Minute, 2).PullFiles(context.Background(), "other", repo(1, "o", "n"), 1)
	if err != nil || len(capped.Files) != 200 || !capped.Truncated {
		t.Fatalf("capped: %d files, truncated %v, err %v", len(capped.Files), capped.Truncated, err)
	}
}

func TestTreeKeepsBlobsAndTreesAndPassesTruncationThrough(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) {
		providertest.JSON(w, `{"sha":"abc","truncated":true,"tree":[
			{"path":"src","type":"tree","mode":"040000"},
			{"path":"src/Main.java","type":"blob","size":120},
			{"path":"vendor/lib","type":"commit"}]}`)
	})
	p := newRESTProvider(t, srv, time.Minute, 5)

	tree, _, err := p.Tree(context.Background(), "tok", repo(1, "o", "n"), "abc123")

	if err != nil || !tree.Truncated || len(tree.Entries) != 2 {
		t.Fatalf("tree = %+v err = %v", tree, err)
	}
	if e := tree.Entries[1]; e.Path != "src/Main.java" || e.Type != "blob" || e.Size != 120 {
		t.Errorf("entry = %+v", e)
	}
	if e := tree.Entries[0]; e.Type != "tree" || e.Size != 0 {
		t.Errorf("entry = %+v", e)
	}
	if reqs := srv.Requests(); reqs[0].Path != "/repos/o/n/git/trees/abc123" || reqs[0].Query != "recursive=1" {
		t.Errorf("request = %+v", reqs[0])
	}
}

func TestFileRequestsRawContentWithEscapedPath(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) {
		_, _ = w.Write([]byte("class A {}\n"))
	})
	p := newRESTProvider(t, srv, time.Minute, 5)

	content, _, err := p.File(context.Background(), "tok", repo(1, "o", "n"), "abc", "src/main/My File#1.java")

	if err != nil || string(content) != "class A {}\n" {
		t.Fatalf("File = %q, %v", content, err)
	}
	req := srv.Requests()[0]
	if req.Path != "/repos/o/n/contents/src/main/My%20File%231.java" || req.Query != "ref=abc" || req.Accept != "application/vnd.github.raw+json" {
		t.Errorf("request = %+v", req)
	}
}

func TestRESTResultsAreCachedAndRevalidatedWithETag(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(req providertest.RESTRequest, w http.ResponseWriter) {
		if req.IfNoneMatch == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("content"))
	})
	fresh := newRESTProvider(t, srv, time.Hour, 5)
	for i := 0; i < 3; i++ {
		if _, _, err := fresh.File(context.Background(), "tok", repo(1, "o", "n"), "abc", "a.txt"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("%d upstream requests within the TTL, want 1", n)
	}

	expired := newRESTProvider(t, srv, time.Nanosecond, 5)
	for i := 0; i < 2; i++ {
		content, _, err := expired.File(context.Background(), "tok2", repo(1, "o", "n"), "abc", "a.txt")
		if err != nil || string(content) != "content" {
			t.Fatalf("File = %q, %v", content, err)
		}
	}
	reqs := srv.Requests()
	if len(reqs) != 3 || reqs[1].IfNoneMatch != "" || reqs[2].IfNoneMatch != `"v1"` {
		t.Fatalf("requests = %+v, want a conditional second request", reqs)
	}
}

func TestRESTErrors(t *testing.T) {
	status := http.StatusNotFound
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) { w.WriteHeader(status) })

	for name, call := range map[string]func(*github.Provider) error{
		"files": func(p *github.Provider) error {
			_, _, err := p.PullFiles(context.Background(), "t", repo(1, "o", "n"), 1)
			return err
		},
		"tree": func(p *github.Provider) error {
			_, _, err := p.Tree(context.Background(), "t", repo(1, "o", "n"), "x")
			return err
		},
		"file": func(p *github.Provider) error {
			_, _, err := p.File(context.Background(), "t", repo(1, "o", "n"), "x", "a")
			return err
		},
	} {
		status = http.StatusNotFound
		if err := call(newRESTProvider(t, srv, time.Minute, 5)); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("%s: 404 error = %v, want ErrNotFound", name, err)
		}
		status = http.StatusForbidden
		if err := call(newRESTProvider(t, srv, time.Minute, 5)); err == nil || errors.Is(err, provider.ErrNotFound) {
			t.Errorf("%s: 403 error = %v", name, err)
		}
	}
}

func TestRESTMalformedBodies(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) { providertest.JSON(w, `{"not":"a list"`) })
	p := newRESTProvider(t, srv, time.Minute, 5)

	if _, _, err := p.PullFiles(context.Background(), "t", repo(1, "o", "n"), 1); err == nil {
		t.Error("malformed files accepted")
	}
	if _, _, err := p.Tree(context.Background(), "t", repo(1, "o", "n"), "x"); err == nil {
		t.Error("malformed tree accepted")
	}
}
