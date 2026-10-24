package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/model"
)

func TestClassify(t *testing.T) {
	entries := []repoEntry{
		{ID: 1, URL: "https://github.com/octo/hello", Owner: "octo", Name: "hello"},
		{ID: 2, URL: "https://gitlab.example.com:8443/group/sub/backend", Owner: "group/sub", Name: "backend"},
		{ID: 3, URL: "http://git.local/pdd/frontend", Owner: "pdd", Name: "frontend"},
		{ID: 4, URL: "https://jira.example.com/browse/X", Type: "jira", Owner: "x", Name: "y"},
		{ID: 5, URL: "https://jenkins.example.com/job/x", Type: "jenkins"},
		{ID: 6, URL: "https://gitlab.example.com/nameless", Owner: "nameless"},
		{ID: 7, URL: "not a url", Owner: "a", Name: "b"},
		{ID: 8, URL: "ftp://files.example.com/a/b", Owner: "a", Name: "b"},
		{ID: 9, URL: "https://WWW.GitHub.com/Octo/Other", Owner: "Octo", Name: "Other"},
	}

	got := classify(entries)

	want := []model.Repo{
		{ID: 1, Provider: model.GitHub, BaseURL: "https://github.com", Owner: "octo", Name: "hello", URL: entries[0].URL},
		{ID: 2, Provider: model.GitLab, BaseURL: "https://gitlab.example.com:8443", Owner: "group/sub", Name: "backend", URL: entries[1].URL},
		{ID: 3, Provider: model.GitLab, BaseURL: "http://git.local", Owner: "pdd", Name: "frontend", URL: entries[2].URL},
		{ID: 9, Provider: model.GitHub, BaseURL: "https://WWW.GitHub.com", Owner: "Octo", Name: "Other", URL: entries[8].URL},
	}
	if len(got) != len(want) {
		t.Fatalf("classify returned %d repos %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("repo %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestClassifyDerivesGitLabBaseFromRepositoryURL(t *testing.T) {
	tests := []struct {
		name, url, owner, repo, base string
	}{
		{"http ip host", "http://140.124.183.111/group/repo", "group", "repo", "http://140.124.183.111"},
		{"https ip with port and nested group", "https://10.0.0.5:8443/a/b/c/repo", "a/b/c", "repo", "https://10.0.0.5:8443"},
		{"trailing .git", "http://140.124.183.111/group/repo.git", "group", "repo", "http://140.124.183.111"},
		{"trailing slash", "https://10.0.0.5:8443/a/b/c/repo/", "a/b/c", "repo", "https://10.0.0.5:8443"},
		{"trailing .git and slash", "https://gitlab.example.com/g/repo.git/", "g", "repo", "https://gitlab.example.com"},
		{"path prefix", "https://example.com/gitlab/team/repo", "team", "repo", "https://example.com/gitlab"},
		{"path prefix, nested group, .git", "http://example.com:8080/tools/gitlab/a/b/repo.git", "a/b", "repo", "http://example.com:8080/tools/gitlab"},
		{"url does not end with owner and name", "https://git.example.com/other/path", "team", "repo", "https://git.example.com"},
		{"query is ignored", "http://10.1.1.1/g/repo?ref=main", "g", "repo", "http://10.1.1.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify([]repoEntry{{ID: 1, URL: tt.url, Owner: tt.owner, Name: tt.repo}})

			if len(got) != 1 || got[0].Provider != model.GitLab || got[0].BaseURL != tt.base || got[0].FullPath() != tt.owner+"/"+tt.repo {
				t.Fatalf("classify = %+v, want GitLab base %q", got, tt.base)
			}
		})
	}
}

type fakeBackend struct {
	*httptest.Server
	projectCalls, repoCalls atomic.Int32
	lastAuth                atomic.Value
	status                  atomic.Int32
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	f := &fakeBackend{}
	f.status.Store(http.StatusOK)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /project/", func(w http.ResponseWriter, r *http.Request) {
		f.projectCalls.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		if s := int(f.status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"a","ownerId":1,"ownerName":"o","members":[]},{"id":7,"name":"b"}]`))
	})
	mux.HandleFunc("GET /repo/{id}", func(w http.ResponseWriter, _ *http.Request) {
		f.repoCalls.Add(1)
		_, _ = w.Write([]byte(`[{"id":10,"url":"https://github.com/o/n","type":null,"owner":"o","name":"n"},{"id":11,"url":"x","type":"jira"}]`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func TestIsMemberForwardsJWTAndCaches(t *testing.T) {
	fake := newFakeBackend(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := New(Config{BaseURL: fake.URL, Timeout: time.Second, CacheTTL: 30 * time.Second, Now: func() time.Time { return now }})
	ctx := context.Background()

	member, err := c.IsMember(ctx, "jwt-1", "alice", 7)
	if err != nil || !member {
		t.Fatalf("IsMember(7) = %v, %v", member, err)
	}
	if got := fake.lastAuth.Load(); got != "Bearer jwt-1" {
		t.Fatalf("Authorization = %v", got)
	}
	if member, _ := c.IsMember(ctx, "jwt-1", "alice", 99); member {
		t.Fatal("non-member reported as member")
	}
	if n := fake.projectCalls.Load(); n != 1 {
		t.Fatalf("backend called %d times within TTL, want 1", n)
	}

	now = now.Add(31 * time.Second)
	if _, err := c.IsMember(ctx, "jwt-2", "alice", 7); err != nil {
		t.Fatal(err)
	}
	if n := fake.projectCalls.Load(); n != 2 {
		t.Fatalf("backend called %d times after TTL, want 2", n)
	}
}

func TestIsMemberCachesPerAccount(t *testing.T) {
	fake := newFakeBackend(t)
	c := New(Config{BaseURL: fake.URL, Timeout: time.Second, CacheTTL: time.Minute})
	_, _ = c.IsMember(context.Background(), "j", "alice", 1)
	_, _ = c.IsMember(context.Background(), "j", "bob", 1)
	if n := fake.projectCalls.Load(); n != 2 {
		t.Fatalf("calls = %d, want one per account", n)
	}
}

func TestIsMemberBackendFailure(t *testing.T) {
	fake := newFakeBackend(t)
	fake.status.Store(http.StatusInternalServerError)
	c := New(Config{BaseURL: fake.URL, Timeout: time.Second, CacheTTL: time.Minute})

	if _, err := c.IsMember(context.Background(), "j", "alice", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

func TestReposClassifiesAndCaches(t *testing.T) {
	fake := newFakeBackend(t)
	c := New(Config{BaseURL: fake.URL, Timeout: time.Second, CacheTTL: time.Minute})

	for i := 0; i < 2; i++ {
		repos, err := c.Repos(context.Background(), "", 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(repos) != 1 || repos[0].ID != 10 || repos[0].Provider != model.GitHub {
			t.Fatalf("repos = %+v", repos)
		}
	}
	if n := fake.repoCalls.Load(); n != 1 {
		t.Fatalf("repo endpoint called %d times, want 1", n)
	}
}

func TestInvalidJSONAndUnreachable(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }))
	defer bad.Close()
	c := New(Config{BaseURL: bad.URL, Timeout: time.Second, CacheTTL: time.Minute})
	if _, err := c.Repos(context.Background(), "", 1); !errors.Is(err, ErrUnavailable) {
		t.Errorf("invalid JSON error = %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	c = New(Config{BaseURL: url, Timeout: time.Second, CacheTTL: time.Minute})
	if _, err := c.Repos(context.Background(), "", 1); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreachable error = %v", err)
	}
}

func TestInsecureTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	defer srv.Close()

	strict := New(Config{BaseURL: srv.URL, Timeout: time.Second, CacheTTL: time.Minute})
	if _, err := strict.Repos(context.Background(), "", 1); !errors.Is(err, ErrUnavailable) {
		t.Errorf("self-signed certificate accepted without InsecureTLS: %v", err)
	}
	lax := New(Config{BaseURL: srv.URL, InsecureTLS: true, Timeout: time.Second, CacheTTL: time.Minute})
	if _, err := lax.Repos(context.Background(), "", 1); err != nil {
		t.Errorf("InsecureTLS rejected the certificate: %v", err)
	}
}
