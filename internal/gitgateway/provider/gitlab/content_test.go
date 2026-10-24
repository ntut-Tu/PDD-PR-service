package gitlab_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/provider/gitlab"
	"pdd/services/internal/gitgateway/provider/providertest"
)

func newRESTProvider(t *testing.T, ttl time.Duration, maxPages int) *gitlab.Provider {
	t.Helper()
	return gitlab.New(gitlab.Config{
		Client: providertest.NewClient(t), DetailTTL: ttl, TreeTTL: ttl, FileTTL: ttl, FilesMaxPages: maxPages, ExpectedCost: 1,
	})
}

func TestPullFilesMapsDiffsAndStatuses(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) {
		providertest.JSON(w, `[
			{"old_path":"A.java","new_path":"A.java","new_file":true,"diff":"@@ -0,0 +1,2 @@\n+a\n+b\n"},
			{"old_path":"old/B.java","new_path":"new/B.java","renamed_file":true,"diff":""},
			{"old_path":"C.java","new_path":"C.java","deleted_file":true,"diff":"@@ -1 +0,0 @@\n-c\n"},
			{"old_path":"D.java","new_path":"D.java","diff":"@@ -1 +1 @@\n-d\n+e\n"}]`)
	})
	p := newRESTProvider(t, time.Minute, 5)

	res, _, err := p.PullFiles(context.Background(), "tok", repo(1, srv.URL, "grp/sub", "app"), 7)

	if err != nil || res.Truncated || len(res.Files) != 4 {
		t.Fatalf("PullFiles = %+v, %v", res, err)
	}
	reqs := srv.Requests()
	if reqs[0].Path != "/api/v4/projects/grp%2Fsub%2Fapp/merge_requests/7/diffs" || reqs[0].Query != "per_page=100&page=1" || reqs[0].Authorization != "Bearer tok" {
		t.Errorf("request = %+v", reqs[0])
	}
	added, renamed, removed, modified := res.Files[0], res.Files[1], res.Files[2], res.Files[3]
	if added.Status != "added" || added.Additions != 2 || added.Deletions != 0 || added.Path != "A.java" {
		t.Errorf("added = %+v", added)
	}
	if renamed.Status != "renamed" || renamed.Path != "new/B.java" || renamed.PreviousPath != "old/B.java" {
		t.Errorf("renamed = %+v", renamed)
	}
	if removed.Status != "removed" || removed.Path != "C.java" || removed.Deletions != 1 {
		t.Errorf("removed = %+v", removed)
	}
	if modified.Status != "modified" || modified.Additions != 1 || modified.Deletions != 1 || modified.PreviousPath != "" {
		t.Errorf("modified = %+v", modified)
	}
}

func treePage(from, n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"x","name":"f","type":"blob","path":"f%d.java","mode":"100644"}`, from+i)
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestTreePaginatesByPageSizeAndFlagsTruncation(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(req providertest.RESTRequest, w http.ResponseWriter) {
		switch {
		case strings.HasSuffix(req.Query, "page=1"):
			providertest.JSON(w, treePage(0, 99)[:len(treePage(0, 99))-1]+`,{"path":"sub","type":"tree"}]`)
		case strings.HasSuffix(req.Query, "page=2"):
			providertest.JSON(w, treePage(100, 99)[:len(treePage(100, 99))-1]+`,{"path":"mod","type":"commit"}]`)
		default:
			providertest.JSON(w, treePage(200, 5))
		}
	})
	r := repo(1, srv.URL, "pdd", "backend")

	tree, _, err := newRESTProvider(t, time.Minute, 5).Tree(context.Background(), "tok", r, "abc")
	if err != nil || tree.Truncated || len(tree.Entries) != 99+1+99+5 {
		t.Fatalf("tree: %d entries, truncated %v, err %v", len(tree.Entries), tree.Truncated, err)
	}
	if e := tree.Entries[99]; e.Path != "sub" || e.Type != "tree" {
		t.Errorf("tree entry = %+v", e)
	}
	if reqs := srv.Requests(); reqs[0].Path != "/api/v4/projects/pdd%2Fbackend/repository/tree" ||
		reqs[0].Query != "recursive=true&ref=abc&per_page=100&page=1" || len(reqs) != 3 {
		t.Errorf("requests = %+v", reqs)
	}

	capped, _, err := newRESTProvider(t, time.Minute, 2).Tree(context.Background(), "tok2", r, "abc")
	if err != nil || !capped.Truncated || len(capped.Entries) != 99+1+99 {
		t.Fatalf("capped: %d entries, truncated %v, err %v", len(capped.Entries), capped.Truncated, err)
	}
}

func TestFileEscapesPathAndProjectAsOneSegment(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) {
		_, _ = w.Write([]byte("class A {}\n"))
	})

	content, _, err := newRESTProvider(t, time.Minute, 5).File(context.Background(), "tok", repo(1, srv.URL, "grp/sub", "app"), "feature/x", "src/main/A B.java")

	if err != nil || string(content) != "class A {}\n" {
		t.Fatalf("File = %q, %v", content, err)
	}
	req := srv.Requests()[0]
	if req.Path != "/api/v4/projects/grp%2Fsub%2Fapp/repository/files/src%2Fmain%2FA%20B.java/raw" || req.Query != "ref=feature%2Fx" {
		t.Errorf("request = %+v", req)
	}
}

func TestRESTResultsAreCachedAndRevalidatedWithETag(t *testing.T) {
	srv := providertest.NewRESTServer(t, func(req providertest.RESTRequest, w http.ResponseWriter) {
		if req.IfNoneMatch == `"t1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"t1"`)
		providertest.JSON(w, treePage(0, 3))
	})
	r := repo(1, srv.URL, "pdd", "backend")
	fresh := newRESTProvider(t, time.Hour, 5)
	for i := 0; i < 3; i++ {
		if _, _, err := fresh.Tree(context.Background(), "tok", r, "abc"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("%d upstream requests within the TTL, want 1", n)
	}

	expired := newRESTProvider(t, time.Nanosecond, 5)
	for i := 0; i < 2; i++ {
		tree, _, err := expired.Tree(context.Background(), "tok2", r, "abc")
		if err != nil || len(tree.Entries) != 3 {
			t.Fatalf("Tree = %+v, %v", tree, err)
		}
	}
	reqs := srv.Requests()
	if len(reqs) != 3 || reqs[2].IfNoneMatch != `"t1"` {
		t.Fatalf("requests = %+v, want a conditional second request", reqs)
	}
}

func TestRESTErrors(t *testing.T) {
	status := http.StatusNotFound
	srv := providertest.NewRESTServer(t, func(_ providertest.RESTRequest, w http.ResponseWriter) { w.WriteHeader(status) })
	r := repo(1, srv.URL, "pdd", "backend")

	for name, call := range map[string]func(*gitlab.Provider) error{
		"files": func(p *gitlab.Provider) error { _, _, err := p.PullFiles(context.Background(), "t", r, 1); return err },
		"tree":  func(p *gitlab.Provider) error { _, _, err := p.Tree(context.Background(), "t", r, "x"); return err },
		"file": func(p *gitlab.Provider) error {
			_, _, err := p.File(context.Background(), "t", r, "x", "a")
			return err
		},
	} {
		status = http.StatusNotFound
		if err := call(newRESTProvider(t, time.Minute, 5)); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("%s: 404 error = %v, want ErrNotFound", name, err)
		}
		status = http.StatusForbidden
		if err := call(newRESTProvider(t, time.Minute, 5)); err == nil || errors.Is(err, provider.ErrNotFound) {
			t.Errorf("%s: 403 error = %v", name, err)
		}
	}
}
