package app_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/app"
	"pdd/services/internal/gitgateway/auth/authtest"
	"pdd/services/internal/gitgateway/config"
)

const (
	jwtSecret     = "integration-secret"
	internalToken = "internal-integration"
)

// world is the fake environment of the gateway: the PDD backend and both providers.
type world struct {
	backend, github, gitlab *httptest.Server

	backendCalls, githubCalls, gitlabCalls atomic.Int32
	githubStatus                           atomic.Int32
	githubRemaining                        atomic.Int32
	lastGitHubAuth, lastGitLabAuth         atomic.Value

	// archiveDownloads counts the requests that delivered archive bytes; flaky counts the
	// requests for the archive that fails once.
	archiveDownloads, flaky atomic.Int32
	lastDownloadAuth        atomic.Value
	lastTarballAuth         atomic.Value
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{}
	w.githubStatus.Store(http.StatusOK)
	w.githubRemaining.Store(4000)

	w.github = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.githubCalls.Add(1)
		w.lastGitHubAuth.Store(r.Header.Get("Authorization"))
		if status := int(w.githubStatus.Load()); status != http.StatusOK {
			rw.WriteHeader(status)
			return
		}
		if w.serveArchive(rw, r, "/repos/octo/hello/tarball/", "/codeload/octo/hello/") {
			return
		}
		if r.Method == http.MethodGet {
			serveGitHubREST(rw, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		rate := fmt.Sprintf(`"rateLimit":{"cost":1,"remaining":%d,"resetAt":"2099-01-01T00:00:00Z","limit":5000}`, w.githubRemaining.Load())
		if strings.Contains(string(body), "query PullDetail") {
			mustFprintf(rw, `{"data":{"repository":{"pullRequest":{"number":42,"title":"GH detail","isDraft":false,"mergeable":"MERGEABLE",`+
				`"url":"https://github.com/octo/hello/pull/42","updatedAt":"2026-10-24T10:00:00Z","headRefName":"f","baseRefName":"main","headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","baseRefOid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",`+
				`"author":{"login":"alice","avatarUrl":"a"},"body":"desc","commits":{"nodes":[]},"comments":{"nodes":[]}}},%s}}`, rate)
			return
		}
		mustFprintf(rw, `{"data":{"r0":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+
			`{"number":42,"title":"GH PR","isDraft":true,"mergeable":"CONFLICTING","url":"https://github.com/octo/hello/pull/42",`+
			`"updatedAt":"2026-10-24T10:00:00Z","headRefName":"f","baseRefName":"main","headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","author":{"login":"alice","avatarUrl":"a"}}]}},%s}}`, rate)
	}))

	w.gitlab = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.gitlabCalls.Add(1)
		w.lastGitLabAuth.Store(r.Header.Get("Authorization"))
		if w.serveArchive(rw, r, "/api/v4/projects/pdd%2Fbackend/repository/archive.tar.gz", "") {
			return
		}
		if r.Method == http.MethodGet {
			serveGitLabREST(rw, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "query MergeRequestDetail") {
			_, _ = io.WriteString(rw, `{"data":{"project":{"mergeRequest":{"iid":"7","title":"GL detail","draft":false,"detailedMergeStatus":"MERGEABLE",`+
				`"webUrl":"https://gl/mr/7","updatedAt":"2026-10-24T11:00:00Z","sourceBranch":"f","targetBranch":"main","diffHeadSha":"cccccccccccccccccccccccccccccccccccccccc","diffRefs":{"baseSha":"dddddddddddddddddddddddddddddddddddddddd"},`+
				`"author":{"username":"bob","avatarUrl":""},"description":"gl desc","commits":{"nodes":[]},"notes":{"nodes":[]}}}}}`)
			return
		}
		_, _ = io.WriteString(rw, `{"data":{"projects":{"nodes":[{"fullPath":"pdd/backend","mergeRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+
			`{"iid":"7","title":"GL MR","draft":false,"detailedMergeStatus":"MERGEABLE","webUrl":"https://gl/mr/7",`+
			`"updatedAt":"2026-10-24T11:00:00Z","sourceBranch":"f","targetBranch":"main","diffHeadSha":"cccccccccccccccccccccccccccccccccccccccc","author":{"username":"bob","avatarUrl":""}}]}}]}}}`)
	}))

	w.backend = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.backendCalls.Add(1)
		switch r.URL.Path {
		case "/project/":
			_, _ = io.WriteString(rw, `[{"id":7,"name":"PDD","ownerId":1,"ownerName":"o","members":[]}]`)
		case "/repo/7":
			mustFprintf(rw, `[{"id":1,"url":"https://github.com/octo/hello","type":null,"owner":"octo","name":"hello"},`+
				`{"id":2,"url":"%s/pdd/backend","type":null,"owner":"pdd","name":"backend"},`+
				`{"id":3,"url":"https://jira.example.com","type":"jira"}]`, w.gitlab.URL)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(func() { w.backend.Close(); w.github.Close(); w.gitlab.Close() })
	return w
}

// serveGitHubREST answers the content endpoints of the fake GitHub.
func serveGitHubREST(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.EscapedPath() {
	case "/repos/octo/hello/pulls/42/files":
		_, _ = io.WriteString(rw, `[{"filename":"src/A.java","status":"added","additions":2,"deletions":0,"patch":"@@ -0,0 +1,2 @@\n+a\n+b"}]`)
	case "/repos/octo/hello/git/trees/" + strings.Repeat("a", 40):
		_, _ = io.WriteString(rw, `{"truncated":false,"tree":[{"path":"src","type":"tree"},{"path":"src/A.java","type":"blob","size":9}]}`)
	case "/repos/octo/hello/contents/src/A.java":
		rw.Header().Set("ETag", `"a1"`)
		_, _ = io.WriteString(rw, "class A {}")
	case "/repos/octo/hello/contents/logo.png":
		_, _ = rw.Write([]byte{0x89, 'P', 'N', 'G', 0, 1, 2})
	case "/repos/octo/hello/contents/big.txt":
		_, _ = io.WriteString(rw, strings.Repeat("x", 100))
	default:
		http.NotFound(rw, r)
	}
}

// serveGitLabREST answers the content endpoints of the fake GitLab.
func serveGitLabREST(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.EscapedPath() {
	case "/api/v4/projects/pdd%2Fbackend/merge_requests/7/diffs":
		_, _ = io.WriteString(rw, `[{"old_path":"B.java","new_path":"B.java","new_file":false,"diff":"@@ -1 +1 @@\n-x\n+y\n"}]`)
	case "/api/v4/projects/pdd%2Fbackend/repository/tree":
		_, _ = io.WriteString(rw, `[{"path":"src","type":"tree"},{"path":"src/B.java","type":"blob"}]`)
	case "/api/v4/projects/pdd%2Fbackend/repository/files/src%2FB.java/raw":
		_, _ = io.WriteString(rw, "class B {}")
	default:
		http.NotFound(rw, r)
	}
}

func (w *world) upstreamCalls() int32 { return w.githubCalls.Load() + w.gitlabCalls.Load() }

func (w *world) config(t *testing.T, dir string, overrides string) config.Config {
	t.Helper()
	base := fmt.Sprintf(`
pdd:
  jwtSecret: %s
  backend: {url: %q, cacheTTL: 1h}
security:
  tokenKey: %s
  internalToken: %s
storage: {dataDir: %q}
github: {apiURL: %q}
retry: {baseDelay: 1ms, maxDelay: 2ms}
cache: {ttl: {prList: 1h, prDetail: 1h}}
`, jwtSecret, w.backend.URL, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))), internalToken, dir, w.github.URL)
	cfg, err := config.LoadYAML([]byte(base), []byte(overrides))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func (w *world) start(t *testing.T, dir string, overrides string) *gateway {
	t.Helper()
	a, err := app.New(context.Background(), w.config(t, dir, overrides), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	srv := httptest.NewServer(a.Handler())
	g := &gateway{app: a, srv: srv}
	t.Cleanup(g.stop)
	return g
}

type gateway struct {
	app *app.App
	srv *httptest.Server
}

func (g *gateway) stop() {
	g.srv.Close()
	_ = g.app.Close()
}

func (g *gateway) get(t *testing.T, path string, header http.Header) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, g.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func userAuth(oauth string, validFor time.Duration) http.Header {
	return http.Header{"Authorization": {"Bearer " + authtest.Sign(jwtSecret, "alice", oauth, time.Now().Add(validFor))}}
}

type listBody struct {
	Pulls []struct {
		RepoID    int64  `json:"repoId"`
		Provider  string `json:"provider"`
		Title     string `json:"title"`
		HeadSHA   string `json:"headSha"`
		Draft     bool   `json:"draft"`
		State     string `json:"state"`
		Mergeable string `json:"mergeable"`
	} `json:"pulls"`
	Stale  bool `json:"stale"`
	Errors []struct {
		RepoID int64  `json:"repoId"`
		Code   string `json:"code"`
	} `json:"errors"`
}

func decodeList(t *testing.T, body []byte) listBody {
	t.Helper()
	var l listBody
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return l
}

func TestListRepeatedWithinTTLMakesNoUpstreamCall(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/pulls", userAuth("gho_user", time.Minute))
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	list := decodeList(t, body)
	if len(list.Pulls) != 2 || list.Stale || len(list.Errors) != 0 {
		t.Fatalf("list = %+v", list)
	}
	byProvider := map[string]string{}
	for _, p := range list.Pulls {
		byProvider[p.Provider] = p.Title
		if p.State != "open" {
			t.Errorf("state = %q", p.State)
		}
	}
	if byProvider["github"] != "GH PR" || byProvider["gitlab"] != "GL MR" {
		t.Errorf("pulls by provider = %v", byProvider)
	}
	if got := w.lastGitHubAuth.Load(); got != "Bearer gho_user" {
		t.Errorf("GitHub Authorization = %v, want the oauth claim", got)
	}
	upstream, backend := w.upstreamCalls(), w.backendCalls.Load()
	if upstream != 2 {
		t.Fatalf("first list made %d provider calls, want 2", upstream)
	}

	for i := 0; i < 3; i++ {
		if status, _ := g.get(t, "/api/v1/projects/7/pulls", userAuth("gho_user", time.Minute)); status != http.StatusOK {
			t.Fatalf("repeat status = %d", status)
		}
	}

	if got := w.upstreamCalls(); got != upstream {
		t.Errorf("repeated lists made %d extra provider calls, want 0", got-upstream)
	}
	if got := w.backendCalls.Load(); got != backend {
		t.Errorf("repeated lists made %d extra backend calls within the backend TTL", got-backend)
	}
}

func TestCacheAndTokenSurviveRestartAndInternalAPIServesAfterJWTExpiry(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	first := w.start(t, dir, "")
	if status, body := first.get(t, "/api/v1/projects/7/repos/2/pulls/7", userAuth("glpat_user", time.Minute)); status != http.StatusOK {
		t.Fatalf("detail status = %d: %s", status, body)
	}
	if status, body := first.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_user", time.Minute)); status != http.StatusOK {
		t.Fatalf("list status = %d: %s", status, body)
	}
	first.stop()
	calls := w.upstreamCalls()

	second := w.start(t, dir, "")

	status, body := second.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_user", time.Minute))
	if status != http.StatusOK || len(decodeList(t, body).Pulls) != 2 {
		t.Fatalf("list after restart = %d %s", status, body)
	}
	status, body = second.get(t, "/internal/v1/projects/7/repos/2/pulls/7?account=alice", http.Header{"X-Internal-Token": {internalToken}})
	if status != http.StatusOK || !strings.Contains(string(body), `"description":"gl desc"`) {
		t.Fatalf("internal detail = %d %s", status, body)
	}
	if got := w.upstreamCalls(); got != calls {
		t.Errorf("restart and internal read made %d provider calls, want 0 (persisted cache)", got-calls)
	}
	if status, _ := second.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_user", -time.Minute)); status != http.StatusUnauthorized {
		t.Errorf("expired JWT status = %d, want 401", status)
	}
}

func TestRepoPullListRequestsOnlyTheSelectedRepository(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/repos/1/pulls", userAuth("gho_user", time.Minute))

	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	list := decodeList(t, body)
	if len(list.Pulls) != 1 || list.Pulls[0].RepoID != 1 || list.Pulls[0].Provider != "github" ||
		list.Pulls[0].HeadSHA != strings.Repeat("a", 40) || len(list.Errors) != 0 {
		t.Fatalf("list = %+v", list)
	}
	if w.githubCalls.Load() != 1 || w.gitlabCalls.Load() != 0 {
		t.Fatalf("provider calls github=%d gitlab=%d, want 1 and 0", w.githubCalls.Load(), w.gitlabCalls.Load())
	}

	status, body = g.get(t, "/api/v1/projects/7/repos/2/pulls", userAuth("gho_user", time.Minute))

	list = decodeList(t, body)
	if status != http.StatusOK || len(list.Pulls) != 1 || list.Pulls[0].RepoID != 2 || list.Pulls[0].Provider != "gitlab" ||
		list.Pulls[0].HeadSHA != strings.Repeat("c", 40) {
		t.Fatalf("gitlab list = %d %+v", status, list)
	}
	if w.githubCalls.Load() != 1 || w.gitlabCalls.Load() != 1 {
		t.Errorf("provider calls github=%d gitlab=%d, want 1 and 1", w.githubCalls.Load(), w.gitlabCalls.Load())
	}
}

func TestRepoPullListRejectsUnknownAndNonGitRepositories(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	for name, path := range map[string]string{
		"unknown repo":  "/api/v1/projects/7/repos/99/pulls",
		"jira repo":     "/api/v1/projects/7/repos/3/pulls",
		"other project": "/api/v1/projects/99/repos/1/pulls",
	} {
		want := http.StatusNotFound
		if name == "other project" {
			want = http.StatusForbidden
		}
		if status, body := g.get(t, path, userAuth("gho_user", time.Minute)); status != want {
			t.Errorf("%s: status = %d, want %d (%s)", name, status, want, body)
		}
	}
	if w.upstreamCalls() != 0 {
		t.Error("rejected requests reached a provider")
	}
}

func TestDetailCarriesHeadAndBaseSHA(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	for path, want := range map[string][2]string{
		"/api/v1/projects/7/repos/1/pulls/42": {strings.Repeat("a", 40), strings.Repeat("b", 40)},
		"/api/v1/projects/7/repos/2/pulls/7":  {strings.Repeat("c", 40), strings.Repeat("d", 40)},
	} {
		status, body := g.get(t, path, userAuth("gho_user", time.Minute))
		var detail struct {
			Pull struct {
				HeadSHA string `json:"headSha"`
				BaseSHA string `json:"baseSha"`
			} `json:"pull"`
		}
		if err := json.Unmarshal(body, &detail); err != nil || status != http.StatusOK {
			t.Fatalf("%s: %d %s (%v)", path, status, body, err)
		}
		if detail.Pull.HeadSHA != want[0] || detail.Pull.BaseSHA != want[1] {
			t.Errorf("%s: shas = %q / %q, want %q / %q", path, detail.Pull.HeadSHA, detail.Pull.BaseSHA, want[0], want[1])
		}
	}
}

func TestInternalAPIRefreshesWithStoredTokenWhenCacheIsCold(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	g.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_first", time.Minute))
	g.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_rotated", time.Minute))

	status, body := g.get(t, "/internal/v1/projects/7/repos/2/pulls/7?account=alice", http.Header{"X-Internal-Token": {internalToken}})

	if status != http.StatusOK || !strings.Contains(string(body), "GL detail") {
		t.Fatalf("internal detail = %d %s", status, body)
	}
	if got := w.lastGitLabAuth.Load(); got != "Bearer glpat_rotated" {
		t.Errorf("provider token used = %v, want the latest stored token", got)
	}
}

func TestProviderUnauthorizedIsReportedPerRepo(t *testing.T) {
	w := newWorld(t)
	w.githubStatus.Store(http.StatusUnauthorized)
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_user", time.Minute))

	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	list := decodeList(t, body)
	if len(list.Pulls) != 1 || list.Pulls[0].Provider != "gitlab" || len(list.Errors) != 1 ||
		list.Errors[0].RepoID != 1 || list.Errors[0].Code != "provider_unauthorized" {
		t.Fatalf("list = %+v", list)
	}
}

func TestQuotaReserveServesStaleListAndRejectsColdDetail(t *testing.T) {
	w := newWorld(t)
	w.githubRemaining.Store(5)
	g := w.start(t, t.TempDir(), "cache: {ttl: {prList: 1ms}}")
	if status, body := g.get(t, "/api/v1/projects/7/pulls", userAuth("gho_user", time.Minute)); status != http.StatusOK {
		t.Fatalf("first list = %d %s", status, body)
	}
	time.Sleep(20 * time.Millisecond)
	githubCalls := w.githubCalls.Load()

	status, body := g.get(t, "/api/v1/projects/7/pulls", userAuth("gho_user", time.Minute))

	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	list := decodeList(t, body)
	if !list.Stale || len(list.Pulls) < 1 {
		t.Fatalf("list = %+v, want stale cached data", list)
	}
	var quotaErrors int
	for _, e := range list.Errors {
		if e.RepoID == 1 && e.Code == "quota_reserve" {
			quotaErrors++
		}
	}
	if quotaErrors != 1 {
		t.Errorf("errors = %+v, want one quota_reserve for repo 1", list.Errors)
	}
	if got := w.githubCalls.Load(); got != githubCalls {
		t.Errorf("GitHub was called %d times while in reserve", got-githubCalls)
	}

	status, body = g.get(t, "/api/v1/projects/7/repos/1/pulls/42", userAuth("gho_user", time.Minute))
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), `"quota_reserve"`) {
		t.Errorf("cold detail in reserve = %d %s, want 503 quota_reserve", status, body)
	}
}

func TestUpstreamFailureWithoutCacheIsBadGateway(t *testing.T) {
	w := newWorld(t)
	w.githubStatus.Store(http.StatusInternalServerError)
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/repos/1/pulls/42", userAuth("gho_user", time.Minute))

	if status != http.StatusBadGateway || !strings.Contains(string(body), `"upstream_failed"`) {
		t.Fatalf("detail = %d %s, want 502 upstream_failed", status, body)
	}
	if got := w.githubCalls.Load(); got != 3 {
		t.Errorf("GitHub was called %d times, want 3 attempts", got)
	}
}

func TestAccessControl(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	tests := []struct {
		name   string
		path   string
		header http.Header
		want   int
	}{
		{"no token", "/api/v1/projects/7/pulls", nil, http.StatusUnauthorized},
		{"bad signature", "/api/v1/projects/7/pulls", http.Header{"Authorization": {"Bearer " + authtest.Sign("x", "alice", "o", time.Now().Add(time.Minute))}}, http.StatusUnauthorized},
		{"other project", "/api/v1/projects/99/pulls", userAuth("o", time.Minute), http.StatusForbidden},
		{"unknown repo", "/api/v1/projects/7/repos/99/pulls/1", userAuth("o", time.Minute), http.StatusNotFound},
		{"internal without token", "/internal/v1/projects/7/repos/1/pulls/1?account=alice", nil, http.StatusUnauthorized},
		{"health", "/healthz", nil, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status, body := g.get(t, tt.path, tt.header); status != tt.want {
				t.Errorf("status = %d, want %d (%s)", status, tt.want, body)
			}
		})
	}
	if w.upstreamCalls() != 0 {
		t.Error("rejected requests reached a provider")
	}
}

func TestNewFailsWhenDataDirIsUnusable(t *testing.T) {
	w := newWorld(t)
	cfg := w.config(t, "/proc/nonexistent/data", "")

	if _, err := app.New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("app.New succeeded with an unusable data dir")
	}
}

func TestRunServesUntilContextCancelled(t *testing.T) {
	w := newWorld(t)
	cfg := w.config(t, t.TempDir(), "server: {listenAddr: \"127.0.0.1:0\"}")
	a, err := app.New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func mustFprintf(w io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		panic(err)
	}
}

func internalGet(g *gateway, t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	status, body := g.get(t, path, http.Header{"X-Internal-Token": {internalToken}})
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	return status, decoded
}

// storeToken makes the gateway remember the provider token of alice, as a normal request does.
func storeToken(g *gateway, t *testing.T) {
	t.Helper()
	if status, body := g.get(t, "/api/v1/projects/7/pulls", userAuth("gho_user", time.Minute)); status != http.StatusOK {
		t.Fatalf("seed request: %d %s", status, body)
	}
}

func TestGitHubContentEndpointsEndToEnd(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)

	status, files := internalGet(g, t, "/internal/v1/projects/7/repos/1/pulls/42/files?account=alice")
	file := files["files"].([]any)[0].(map[string]any)
	if status != http.StatusOK || files["headSha"] != head || files["baseSha"] != base || file["path"] != "src/A.java" ||
		file["status"] != "added" || file["additions"] != float64(2) || files["truncated"] != false {
		t.Fatalf("files = %d %v", status, files)
	}

	status, tree := internalGet(g, t, "/internal/v1/projects/7/repos/1/tree?account=alice&ref="+head)
	if status != http.StatusOK || tree["ref"] != head || len(tree["entries"].([]any)) != 2 {
		t.Fatalf("tree = %d %v", status, tree)
	}

	status, content := internalGet(g, t, "/internal/v1/projects/7/repos/1/files?account=alice&ref="+head+"&path=src%2FA.java")
	if status != http.StatusOK || content["content"] != "class A {}" || content["size"] != float64(10) ||
		content["binary"] != false || content["truncated"] != false {
		t.Fatalf("file = %d %v", status, content)
	}
	if got := w.lastGitHubAuth.Load(); got != "Bearer gho_user" {
		t.Errorf("GitHub Authorization = %v, want the stored token", got)
	}
}

func TestPublicTreeIsReadAtThePullRequestHead(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/repos/1/pulls/42/tree", userAuth("gho_user", time.Minute))

	var tree struct {
		Ref     string `json:"ref"`
		Entries []struct {
			Path, Type string
			Size       int64
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &tree); err != nil || status != http.StatusOK {
		t.Fatalf("%d %s (%v)", status, body, err)
	}
	if tree.Ref != strings.Repeat("a", 40) || len(tree.Entries) != 2 || tree.Entries[1].Path != "src/A.java" || tree.Entries[1].Size != 9 {
		t.Errorf("tree = %+v", tree)
	}
}

func TestContentIsServedFromCacheAfterTheFirstRead(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)
	path := "/internal/v1/projects/7/repos/1/files?account=alice&ref=" + strings.Repeat("a", 40) + "&path=src%2FA.java"
	internalGet(g, t, path)
	calls := w.upstreamCalls()

	for i := 0; i < 3; i++ {
		if status, _ := internalGet(g, t, path); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
	}

	if got := w.upstreamCalls(); got != calls {
		t.Errorf("repeated reads made %d provider calls, want 0", got-calls)
	}
}

func TestBinaryAndOversizedFiles(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "files: {maxBytes: 10}")
	storeToken(g, t)
	prefix := "/internal/v1/projects/7/repos/1/files?account=alice&ref=" + strings.Repeat("a", 40) + "&path="

	_, binary := internalGet(g, t, prefix+"logo.png")
	if binary["binary"] != true || binary["content"] != "" || binary["size"] != float64(7) {
		t.Errorf("binary = %v", binary)
	}
	_, big := internalGet(g, t, prefix+"big.txt")
	if big["truncated"] != true || big["content"] != strings.Repeat("x", 10) || big["size"] != float64(100) {
		t.Errorf("big = %v", big)
	}
	status, missing := internalGet(g, t, prefix+"nope.txt")
	if status != http.StatusNotFound {
		t.Errorf("missing file: %d %v", status, missing)
	}
}

func TestGitLabContentEndpointsEndToEnd(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)

	status, files := internalGet(g, t, "/internal/v1/projects/7/repos/2/pulls/7/files?account=alice")
	file := files["files"].([]any)[0].(map[string]any)
	if status != http.StatusOK || files["headSha"] != strings.Repeat("c", 40) || files["baseSha"] != strings.Repeat("d", 40) ||
		file["status"] != "modified" || file["additions"] != float64(1) || file["deletions"] != float64(1) {
		t.Fatalf("files = %d %v", status, files)
	}
	status, tree := internalGet(g, t, "/internal/v1/projects/7/repos/2/tree?account=alice&ref=abc")
	if status != http.StatusOK || len(tree["entries"].([]any)) != 2 {
		t.Fatalf("tree = %d %v", status, tree)
	}
	status, content := internalGet(g, t, "/internal/v1/projects/7/repos/2/files?account=alice&ref=abc&path=src%2FB.java")
	if status != http.StatusOK || content["content"] != "class B {}" {
		t.Fatalf("file = %d %v", status, content)
	}
}

func TestContentEndpointsAccessControl(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	tests := []struct {
		name, path string
		header     http.Header
		want       int
	}{
		{"internal without token", "/internal/v1/projects/7/repos/1/tree?account=alice&ref=x", nil, http.StatusUnauthorized},
		{"internal wrong token", "/internal/v1/projects/7/repos/1/tree?account=alice&ref=x", http.Header{"X-Internal-Token": {"nope"}}, http.StatusUnauthorized},
		{"internal unknown account", "/internal/v1/projects/7/repos/1/tree?account=ghost&ref=x", http.Header{"X-Internal-Token": {internalToken}}, http.StatusNotFound},
		{"public tree without jwt", "/api/v1/projects/7/repos/1/pulls/42/tree", nil, http.StatusUnauthorized},
		{"public tree other project", "/api/v1/projects/99/repos/1/pulls/42/tree", userAuth("o", time.Minute), http.StatusForbidden},
		{"public tree jira repo", "/api/v1/projects/7/repos/3/pulls/42/tree", userAuth("o", time.Minute), http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status, body := g.get(t, tt.path, tt.header); status != tt.want {
				t.Errorf("status = %d, want %d (%s)", status, tt.want, body)
			}
		})
	}
	if w.githubCalls.Load()+w.gitlabCalls.Load() != 0 {
		t.Error("rejected requests reached a provider")
	}
}

func TestPublicChangedFilesUseTheCallersToken(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/repos/1/pulls/42/files", userAuth("gho_user", time.Minute))

	var files struct {
		HeadSHA   string `json:"headSha"`
		BaseSHA   string `json:"baseSha"`
		Stale     bool   `json:"stale"`
		FetchedAt string `json:"fetchedAt"`
		Files     []struct {
			Path, Status string
			Additions    int
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &files); err != nil || status != http.StatusOK {
		t.Fatalf("%d %s (%v)", status, body, err)
	}
	if files.HeadSHA != strings.Repeat("a", 40) || files.BaseSHA != strings.Repeat("b", 40) || files.Stale || files.FetchedAt == "" ||
		len(files.Files) != 1 || files.Files[0].Path != "src/A.java" || files.Files[0].Status != "added" || files.Files[0].Additions != 2 {
		t.Errorf("files = %+v", files)
	}
	if got := w.lastGitHubAuth.Load(); got != "Bearer gho_user" {
		t.Errorf("GitHub Authorization = %v, want the caller's token", got)
	}
	calls := w.upstreamCalls()
	g.get(t, "/api/v1/projects/7/repos/1/pulls/42/files", userAuth("gho_user", time.Minute))
	if w.upstreamCalls() != calls {
		t.Error("second read was not served from the cache")
	}
}

func TestPublicChangedFilesAccessControl(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	for name, tt := range map[string]struct {
		path   string
		header http.Header
		want   int
	}{
		"no jwt":        {"/api/v1/projects/7/repos/1/pulls/42/files", nil, http.StatusUnauthorized},
		"other project": {"/api/v1/projects/99/repos/1/pulls/42/files", userAuth("o", time.Minute), http.StatusForbidden},
		"jira repo":     {"/api/v1/projects/7/repos/3/pulls/42/files", userAuth("o", time.Minute), http.StatusNotFound},
		"unknown repo":  {"/api/v1/projects/7/repos/99/pulls/42/files", userAuth("o", time.Minute), http.StatusNotFound},
	} {
		if status, body := g.get(t, tt.path, tt.header); status != tt.want {
			t.Errorf("%s: status = %d, want %d (%s)", name, status, tt.want, body)
		}
	}
	if w.upstreamCalls() != 0 {
		t.Error("rejected requests reached a provider")
	}
}

// TestSelfHostedGitLabRepositoryIsListed covers a repository on an IP host over http, in a nested
// group, under a path prefix, with a .git suffix and a trailing slash.
func TestSelfHostedGitLabRepositoryIsListed(t *testing.T) {
	var gitlabPath, gitlabToken, gitlabPaths atomic.Value
	gitlab := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gitlabPath.Store(r.URL.Path)
		gitlabToken.Store(r.Header.Get("Authorization"))
		var req struct {
			Variables struct {
				Paths []string `json:"paths"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gitlabPaths.Store(req.Variables.Paths)
		_, _ = io.WriteString(rw, `{"data":{"projects":{"nodes":[{"fullPath":"a/b/c/repo","mergeRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+
			`{"iid":"3","title":"Self-hosted MR","draft":false,"detailedMergeStatus":"MERGEABLE","webUrl":"http://10.0.0.5/a/b/c/repo/-/merge_requests/3",`+
			`"updatedAt":"2026-10-24T11:00:00Z","sourceBranch":"f","targetBranch":"main","author":{"username":"bob","avatarUrl":"/a.png"}}]}}]}}}`)
	}))
	t.Cleanup(gitlab.Close)
	backend := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/project/":
			_, _ = io.WriteString(rw, `[{"id":7,"name":"PDD","ownerId":1,"ownerName":"o","members":[]}]`)
		case "/repo/7":
			mustFprintf(rw, `[{"id":5,"url":"%s/gitlab/a/b/c/repo.git/","type":null,"owner":"a/b/c","name":"repo"}]`, gitlab.URL)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(backend.Close)
	w := newWorld(t)
	w.backend = backend
	g := w.start(t, t.TempDir(), "")

	status, body := g.get(t, "/api/v1/projects/7/pulls", userAuth("glpat_user", time.Minute))

	list := decodeList(t, body)
	if status != http.StatusOK || len(list.Pulls) != 1 || len(list.Errors) != 0 || list.Pulls[0].Title != "Self-hosted MR" ||
		list.Pulls[0].Provider != "gitlab" || list.Pulls[0].RepoID != 5 {
		t.Fatalf("list = %d %s", status, body)
	}
	if got := gitlabPath.Load(); got != "/gitlab/api/graphql" {
		t.Errorf("GitLab endpoint path = %v, want the path prefix kept", got)
	}
	if got := gitlabPaths.Load(); len(got.([]string)) != 1 || got.([]string)[0] != "a/b/c/repo" {
		t.Errorf("project paths = %v", got)
	}
	if got := gitlabToken.Load(); got != "Bearer glpat_user" {
		t.Errorf("GitLab Authorization = %v", got)
	}
}
