package app_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Commits of the fake providers; the first character selects how the fake answers:
// a is a real tar.gz, 1 to 3 are small archives, f is larger than archive.maxBytes
// (announced), e is larger and chunked, 5 fails once with 503, 6 reports an exhausted quota,
// 9 does not exist.
var (
	shaReal    = strings.Repeat("a", 40)
	shaSmall1  = strings.Repeat("1", 40)
	shaSmall2  = strings.Repeat("2", 40)
	shaSmall3  = strings.Repeat("3", 40)
	shaBig     = strings.Repeat("f", 40)
	shaChunked = strings.Repeat("e", 40)
	shaFlaky   = strings.Repeat("5", 40)
	shaQuota   = strings.Repeat("6", 40)
	shaMissing = strings.Repeat("9", 40)
)

const (
	smallArchiveBytes = 60
	archiveLimits     = "archive: {maxBytes: 100, cacheMaxBytes: 150}\n"
)

func realArchive() []byte {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	content := []byte("class A {}")
	_ = tw.WriteHeader(&tar.Header{Name: "octo-hello-aaaaaaa/src/A.java", Mode: 0o644, Size: int64(len(content))})
	_, _ = tw.Write(content)
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

// serveArchive answers the archive routes of a fake provider. prefix is the request path of
// the archive; for GitHub the download then redirects to redirectPrefix on another host name.
// It reports whether it handled the request.
func (w *world) serveArchive(rw http.ResponseWriter, r *http.Request, prefix, redirectPrefix string) bool {
	path := r.URL.EscapedPath()
	if redirectPrefix != "" && strings.HasPrefix(path, redirectPrefix) {
		w.lastDownloadAuth.Store(r.Header.Get("Authorization"))
		w.archiveDownloads.Add(1)
		_, _ = rw.Write(archivePayload(strings.TrimPrefix(path, redirectPrefix)))
		return true
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	w.lastTarballAuth.Store(r.Header.Get("Authorization"))
	sha := strings.TrimPrefix(path, prefix)
	if redirectPrefix == "" {
		sha = r.URL.Query().Get("sha")
	}
	switch sha[0] {
	case '9':
		rw.WriteHeader(http.StatusNotFound)
	case '5':
		if w.flaky.Add(1) == 1 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		w.deliver(rw, r, redirectPrefix, sha)
	case '6':
		rw.Header().Set("X-Ratelimit-Limit", "5000")
		rw.Header().Set("X-Ratelimit-Remaining", "1")
		rw.Header().Set("X-Ratelimit-Reset", "4102444800")
		w.deliver(rw, r, redirectPrefix, sha)
	default:
		w.deliver(rw, r, redirectPrefix, sha)
	}
	return true
}

func (w *world) deliver(rw http.ResponseWriter, r *http.Request, redirectPrefix, sha string) {
	if redirectPrefix != "" {
		// Another host name, so Go drops the Authorization header on the redirect.
		target := strings.Replace("http://"+r.Host, "127.0.0.1", "localhost", 1) + redirectPrefix + sha
		http.Redirect(rw, r, target, http.StatusFound)
		return
	}
	w.lastDownloadAuth.Store(r.Header.Get("Authorization"))
	w.archiveDownloads.Add(1)
	payload := archivePayload(sha)
	if sha[0] == 'e' {
		_, _ = rw.Write(payload[:50])
		rw.(http.Flusher).Flush()
		_, _ = rw.Write(payload[50:])
		return
	}
	_, _ = rw.Write(payload)
}

func archivePayload(sha string) []byte {
	switch sha[0] {
	case 'a':
		return realArchive()
	case 'f':
		return bytes.Repeat([]byte{'f'}, 101)
	case 'e':
		return bytes.Repeat([]byte{'e'}, 101)
	default:
		return bytes.Repeat([]byte{sha[0]}, smallArchiveBytes)
	}
}

func archivePath(repoID, sha string) string {
	return "/internal/v1/projects/7/repos/" + repoID + "/archive?account=alice&ref=" + sha
}

// reply is the status line and headers of a response whose body was read and closed.
type reply struct {
	StatusCode int
	Header     http.Header
}

func rawGet(g *gateway, t *testing.T, path string) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, g.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Internal-Token", internalToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, body
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var decoded struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return decoded.Error.Code
}

func TestGitHubArchiveEndToEnd(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)

	resp, body := rawGet(g, t, archivePath("1", shaReal))

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("status %d type %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	header, err := tar.NewReader(gz).Next()
	if err != nil || header.Name != "octo-hello-aaaaaaa/src/A.java" {
		t.Errorf("first entry = %+v, %v", header, err)
	}
	if got := w.lastTarballAuth.Load(); got != "Bearer gho_user" {
		t.Errorf("tarball request Authorization = %v, want the stored token of the account", got)
	}
	if got := w.lastDownloadAuth.Load(); got != "" {
		t.Errorf("download host received Authorization %v", got)
	}
}

func TestGitLabArchiveEndToEnd(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)

	resp, body := rawGet(g, t, archivePath("2", shaSmall1))

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/gzip" || len(body) != smallArchiveBytes {
		t.Fatalf("status %d type %q length %d", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if got := w.lastDownloadAuth.Load(); got != "Bearer gho_user" {
		t.Errorf("GitLab archive Authorization = %v", got)
	}
}

func TestArchiveIsServedFromDiskAfterTheFirstDownloadAndAfterARestart(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	g := w.start(t, dir, "")
	storeToken(g, t)
	rawGet(g, t, archivePath("2", shaSmall1))
	calls := w.upstreamCalls()

	for range 3 {
		if resp, _ := rawGet(g, t, archivePath("2", shaSmall1)); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	g.stop()
	restarted := w.start(t, dir, "")
	resp, body := rawGet(restarted, t, archivePath("2", shaSmall1))

	if resp.StatusCode != http.StatusOK || len(body) != smallArchiveBytes {
		t.Fatalf("after restart: status %d length %d", resp.StatusCode, len(body))
	}
	if got := w.upstreamCalls(); got != calls || w.archiveDownloads.Load() != 1 {
		t.Errorf("repeated reads made %d provider calls and %d downloads, want 0 and 1", got-calls, w.archiveDownloads.Load())
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "archives")); len(entries) != 1 {
		t.Errorf("archive directory = %v", entries)
	}
}

func TestArchiveEvictsTheLeastRecentlyUsedWhenTheCacheIsFull(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), archiveLimits)
	storeToken(g, t)
	for _, sha := range []string{shaSmall1, shaSmall2, shaSmall1, shaSmall3} {
		if resp, _ := rawGet(g, t, archivePath("2", sha)); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", sha[:1], resp.StatusCode)
		}
	}
	downloads := w.archiveDownloads.Load()

	rawGet(g, t, archivePath("2", shaSmall1))
	rawGet(g, t, archivePath("2", shaSmall3))
	if got := w.archiveDownloads.Load(); got != downloads {
		t.Errorf("recently used archives were evicted: %d new downloads", got-downloads)
	}
	rawGet(g, t, archivePath("2", shaSmall2))

	if got := w.archiveDownloads.Load(); got != downloads+1 {
		t.Errorf("least recently used archive was not evicted: %d new downloads, want 1", got-downloads)
	}
}

func TestArchiveOverTheSizeLimitIsRefusedAndNotKept(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	g := w.start(t, dir, archiveLimits)
	storeToken(g, t)

	for name, tt := range map[string]struct{ repo, sha string }{
		"announced github": {"1", shaBig}, "announced gitlab": {"2", shaBig}, "chunked gitlab": {"2", shaChunked},
	} {
		resp, body := rawGet(g, t, archivePath(tt.repo, tt.sha))
		if resp.StatusCode != http.StatusRequestEntityTooLarge || errorCode(t, body) != "archive_too_large" {
			t.Errorf("%s: status %d body %s", name, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s: Content-Type %q", name, resp.Header.Get("Content-Type"))
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "archives")); len(entries) != 0 {
		t.Errorf("archive directory = %v", entries)
	}
}

func TestArchiveRejectsBadRefsAndUnauthorizedCallersWithoutAProviderCall(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)
	calls := w.upstreamCalls()
	internal := http.Header{"X-Internal-Token": {internalToken}}

	tests := []struct {
		name, path string
		header     http.Header
		status     int
		code       string
	}{
		{"branch as ref", archivePath("1", "main"), internal, http.StatusBadRequest, "bad_request"},
		{"short ref", archivePath("1", shaReal[:39]), internal, http.StatusBadRequest, "bad_request"},
		{"missing ref", "/internal/v1/projects/7/repos/1/archive?account=alice", internal, http.StatusBadRequest, "bad_request"},
		{"missing account", "/internal/v1/projects/7/repos/1/archive?ref=" + shaReal, internal, http.StatusBadRequest, "bad_request"},
		{"no token", archivePath("1", shaReal), nil, http.StatusUnauthorized, "unauthorized"},
		{"wrong token", archivePath("1", shaReal), http.Header{"X-Internal-Token": {"nope"}}, http.StatusUnauthorized, "unauthorized"},
		{"user jwt", archivePath("1", shaReal), userAuth("gho_user", time.Minute), http.StatusUnauthorized, "unauthorized"},
		{"unknown account", "/internal/v1/projects/7/repos/1/archive?account=ghost&ref=" + shaReal, internal, http.StatusNotFound, "not_found"},
		{"repo of no provider", archivePath("3", shaReal), internal, http.StatusNotFound, "not_found"},
		{"unknown repo", archivePath("99", shaReal), internal, http.StatusNotFound, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header = tt.header.Clone()
			g.app.Handler().ServeHTTP(rec, req)
			if rec.Code != tt.status || errorCode(t, rec.Body.Bytes()) != tt.code {
				t.Errorf("status %d body %s, want %d %s", rec.Code, rec.Body, tt.status, tt.code)
			}
		})
	}
	if got := w.upstreamCalls(); got != calls {
		t.Errorf("rejected requests made %d provider calls", got-calls)
	}
}

func TestArchiveIsNotPublic(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")

	status, _ := g.get(t, "/api/v1/projects/7/repos/1/archive?ref="+shaReal, userAuth("gho_user", time.Minute))

	if status != http.StatusNotFound {
		t.Errorf("status = %d", status)
	}
}

func TestArchiveProviderFailuresUseTheStandardErrors(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)

	resp, body := rawGet(g, t, archivePath("1", shaMissing))
	if resp.StatusCode != http.StatusNotFound || errorCode(t, body) != "not_found" {
		t.Errorf("missing commit: %d %s", resp.StatusCode, body)
	}
	w.githubStatus.Store(http.StatusBadGateway)
	resp, body = rawGet(g, t, archivePath("1", shaSmall1))
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_failed" {
		t.Errorf("provider down: %d %s", resp.StatusCode, body)
	}
}

func TestArchiveRequestsFollowTheRetryPolicy(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)

	resp, body := rawGet(g, t, archivePath("2", shaFlaky))

	if resp.StatusCode != http.StatusOK || len(body) != smallArchiveBytes {
		t.Fatalf("status %d length %d", resp.StatusCode, len(body))
	}
	if w.flaky.Load() != 2 {
		t.Errorf("provider saw %d requests, want a failure and a retry", w.flaky.Load())
	}
}

func TestArchiveRequestsStopWhenTheQuotaIsInReserve(t *testing.T) {
	w := newWorld(t)
	g := w.start(t, t.TempDir(), "")
	storeToken(g, t)
	if resp, _ := rawGet(g, t, archivePath("2", shaQuota)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	calls := w.upstreamCalls()

	resp, body := rawGet(g, t, archivePath("2", shaSmall2))

	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "quota_reserve" {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	if got := w.upstreamCalls(); got != calls {
		t.Errorf("a request was sent while the quota was in reserve")
	}
}
