package api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/pulls"
	"pdd/services/internal/gitgateway/repos"
)

const (
	publicTreePath   = "/api/v1/projects/7/repos/12/pulls/42/tree"
	publicFilesPath  = "/api/v1/projects/7/repos/12/pulls/42/files"
	internalFiles    = "/internal/v1/projects/7/repos/12/pulls/42/files?account=alice"
	internalTreePath = "/internal/v1/projects/7/repos/12/tree?account=alice&ref=abc"
	internalFilePath = "/internal/v1/projects/7/repos/12/files?account=alice&ref=abc&path=src%2FA.java"
)

func storedAlice(f *fixture) { f.tokens.saved["alice"] = "gho_alice" }

func TestPublicTree(t *testing.T) {
	f := newFixture(t)
	f.pulls.tree = repos.TreeResult{
		Ref: "headsha", Entries: []model.TreeEntry{{Path: "src/A.java", Type: "blob", Size: 12}}, Truncated: true, Stale: true, FetchedAt: now,
	}

	rec := f.do(http.MethodGet, publicTreePath, validAuth())

	assertOK(t, rec)
	body := decode[map[string]any](t, rec)
	entry := body["entries"].([]any)[0].(map[string]any)
	if body["ref"] != "headsha" || body["truncated"] != true || body["stale"] != true || body["fetchedAt"] != "2026-10-24T12:00:00Z" ||
		entry["path"] != "src/A.java" || entry["type"] != "blob" || entry["size"] != float64(12) {
		t.Errorf("body = %v", body)
	}
	if f.pulls.gotToken != "gho_alice" || f.pulls.gotNumber != 42 || f.pulls.gotRepo.ID != 12 {
		t.Errorf("provider call token %q number %d repo %+v", f.pulls.gotToken, f.pulls.gotNumber, f.pulls.gotRepo)
	}
}

func TestPublicTreeEmptyEncodesAsList(t *testing.T) {
	f := newFixture(t)

	rec := f.do(http.MethodGet, publicTreePath, validAuth())

	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestPublicTreeAccessControl(t *testing.T) {
	f := newFixture(t)

	assertError(t, f.do(http.MethodGet, publicTreePath, nil), http.StatusUnauthorized, "unauthorized")
	assertError(t, f.do(http.MethodGet, "/api/v1/projects/8/repos/12/pulls/42/tree", validAuth()), http.StatusForbidden, "forbidden")
	for _, path := range []string{
		"/api/v1/projects/7/repos/99/pulls/42/tree", "/api/v1/projects/7/repos/x/pulls/42/tree",
		"/api/v1/projects/7/repos/12/pulls/x/tree", "/api/v1/projects/x/repos/12/pulls/42/tree",
	} {
		assertError(t, f.do(http.MethodGet, path, validAuth()), http.StatusNotFound, "not_found")
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called for a rejected request")
	}
}

func TestPublicTreeProviderErrors(t *testing.T) {
	f := newFixture(t)
	f.pulls.contentEr = provider.ErrNotFound
	assertError(t, f.do(http.MethodGet, publicTreePath, validAuth()), http.StatusNotFound, "not_found")

	f.pulls.contentEr = &statusErr{}
	assertError(t, f.do(http.MethodGet, publicTreePath, validAuth()), http.StatusBadGateway, "upstream_failed")
}

type statusErr struct{}

func (*statusErr) Error() string { return "boom" }

func TestPublicFiles(t *testing.T) {
	f := newFixture(t)
	f.pulls.files = pulls.FilesResult{
		HeadSHA: "head", BaseSHA: "base", Stale: true, FetchedAt: now,
		Files: []model.ChangedFile{{Path: "src/A.java", Status: "modified", Additions: 3, Deletions: 1, Patch: "@@"}},
	}

	rec := f.do(http.MethodGet, publicFilesPath, validAuth())

	assertOK(t, rec)
	body := decode[map[string]any](t, rec)
	file := body["files"].([]any)[0].(map[string]any)
	if body["headSha"] != "head" || body["baseSha"] != "base" || body["truncated"] != false || body["stale"] != true ||
		body["fetchedAt"] != "2026-10-24T12:00:00Z" || file["path"] != "src/A.java" || file["status"] != "modified" || file["patch"] != "@@" {
		t.Errorf("body = %v", body)
	}
	if f.pulls.gotToken != "gho_alice" || f.pulls.gotNumber != 42 || f.pulls.gotRepo.ID != 12 {
		t.Errorf("provider call token %q number %d repo %+v", f.pulls.gotToken, f.pulls.gotNumber, f.pulls.gotRepo)
	}
}

func TestPublicFilesEmptyEncodesAsList(t *testing.T) {
	rec := newFixture(t).do(http.MethodGet, publicFilesPath, validAuth())

	if !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestPublicFilesAccessControlAndErrors(t *testing.T) {
	f := newFixture(t)

	assertError(t, f.do(http.MethodGet, publicFilesPath, nil), http.StatusUnauthorized, "unauthorized")
	assertError(t, f.do(http.MethodGet, "/api/v1/projects/8/repos/12/pulls/42/files", validAuth()), http.StatusForbidden, "forbidden")
	for _, path := range []string{
		"/api/v1/projects/7/repos/99/pulls/42/files", "/api/v1/projects/7/repos/x/pulls/42/files",
		"/api/v1/projects/7/repos/12/pulls/x/files", "/api/v1/projects/x/repos/12/pulls/42/files",
	} {
		assertError(t, f.do(http.MethodGet, path, validAuth()), http.StatusNotFound, "not_found")
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called for a rejected request")
	}
	f.pulls.contentEr = provider.ErrNotFound
	assertError(t, f.do(http.MethodGet, publicFilesPath, validAuth()), http.StatusNotFound, "not_found")
	f.pulls.contentEr = &statusErr{}
	assertError(t, f.do(http.MethodGet, publicFilesPath, validAuth()), http.StatusBadGateway, "upstream_failed")
}

func TestInternalFiles(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	f.pulls.files = pulls.FilesResult{
		HeadSHA: "head", BaseSHA: "base", Truncated: true,
		Files: []model.ChangedFile{{Path: "n.java", PreviousPath: "o.java", Status: "renamed", Additions: 1, Deletions: 2, Patch: "@@"}},
	}

	rec := f.do(http.MethodGet, internalFiles, internalHeader(internalToken))

	assertOK(t, rec)
	body := decode[map[string]any](t, rec)
	file := body["files"].([]any)[0].(map[string]any)
	if body["headSha"] != "head" || body["baseSha"] != "base" || body["truncated"] != true ||
		file["path"] != "n.java" || file["previousPath"] != "o.java" || file["status"] != "renamed" ||
		file["additions"] != float64(1) || file["deletions"] != float64(2) || file["patch"] != "@@" {
		t.Errorf("body = %v", body)
	}
	if f.pulls.gotToken != "gho_alice" || f.pulls.gotNumber != 42 || f.backend.reposJWT != "" {
		t.Errorf("token %q number %d jwt %q", f.pulls.gotToken, f.pulls.gotNumber, f.backend.reposJWT)
	}
}

func TestInternalFilesEmptyEncodesAsList(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)

	rec := f.do(http.MethodGet, internalFiles, internalHeader(internalToken))

	if !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestInternalTree(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	f.pulls.tree = repos.TreeResult{Ref: "abc", Entries: []model.TreeEntry{{Path: "a", Type: "tree"}}, FetchedAt: now}

	rec := f.do(http.MethodGet, internalTreePath, internalHeader(internalToken))

	assertOK(t, rec)
	body := decode[map[string]any](t, rec)
	if body["ref"] != "abc" || len(body["entries"].([]any)) != 1 || body["truncated"] != false || body["stale"] != false {
		t.Errorf("body = %v", body)
	}
	if f.pulls.gotRef != "abc" || f.pulls.gotToken != "gho_alice" || f.pulls.gotRepo.ID != 12 {
		t.Errorf("ref %q token %q repo %+v", f.pulls.gotRef, f.pulls.gotToken, f.pulls.gotRepo)
	}
}

func TestInternalFile(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	f.pulls.file = repos.FileResult{Ref: "abc", Path: "src/A.java", Size: 9, Content: "class A{}", Truncated: true}

	rec := f.do(http.MethodGet, internalFilePath, internalHeader(internalToken))

	assertOK(t, rec)
	body := decode[map[string]any](t, rec)
	if body["ref"] != "abc" || body["path"] != "src/A.java" || body["size"] != float64(9) || body["content"] != "class A{}" ||
		body["binary"] != false || body["truncated"] != true {
		t.Errorf("body = %v", body)
	}
	if f.pulls.gotPath != "src/A.java" || f.pulls.gotRef != "abc" {
		t.Errorf("ref %q path %q", f.pulls.gotRef, f.pulls.gotPath)
	}
}

func TestInternalFileBinaryHasEmptyContent(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	f.pulls.file = repos.FileResult{Ref: "abc", Path: "a.png", Size: 500, Binary: true}

	body := decode[map[string]any](t, f.do(http.MethodGet, internalFilePath, internalHeader(internalToken)))

	if body["binary"] != true || body["content"] != "" || body["size"] != float64(500) {
		t.Errorf("body = %v", body)
	}
}

func TestInternalContentEndpointsRequireTheInternalToken(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)

	for _, path := range []string{internalFiles, internalTreePath, internalFilePath} {
		for name, header := range map[string]http.Header{
			"missing": nil, "wrong": internalHeader("nope"), "user jwt": validAuth(),
		} {
			if rec := f.do(http.MethodGet, path, header); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s with %s token: status %d", path, name, rec.Code)
			}
		}
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called without a valid internal token")
	}
}

func TestInternalContentEndpointsRejectBadRequests(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	h := internalHeader(internalToken)

	tests := []struct {
		name, path string
		status     int
		code       string
	}{
		{"files without account", "/internal/v1/projects/7/repos/12/pulls/42/files", http.StatusBadRequest, "bad_request"},
		{"unknown account", "/internal/v1/projects/7/repos/12/pulls/42/files?account=bob", http.StatusNotFound, "not_found"},
		{"tree unknown account", "/internal/v1/projects/7/repos/12/tree?account=bob&ref=abc", http.StatusNotFound, "not_found"},
		{"file unknown account", "/internal/v1/projects/7/repos/12/files?account=bob&ref=abc&path=a", http.StatusNotFound, "not_found"},
		{"files unknown repo", "/internal/v1/projects/7/repos/99/pulls/42/files?account=alice", http.StatusNotFound, "not_found"},
		{"files bad number", "/internal/v1/projects/7/repos/12/pulls/x/files?account=alice", http.StatusNotFound, "not_found"},
		{"tree unknown repo", "/internal/v1/projects/7/repos/99/tree?account=alice&ref=abc", http.StatusNotFound, "not_found"},
		{"file unknown repo", "/internal/v1/projects/7/repos/99/files?account=alice&ref=abc&path=a", http.StatusNotFound, "not_found"},
		{"bad project", "/internal/v1/projects/x/repos/12/tree?account=alice&ref=abc", http.StatusNotFound, "not_found"},
		{"tree without ref", "/internal/v1/projects/7/repos/12/tree?account=alice", http.StatusBadRequest, "bad_request"},
		{"file without ref", "/internal/v1/projects/7/repos/12/files?account=alice&path=a", http.StatusBadRequest, "bad_request"},
		{"file without path", "/internal/v1/projects/7/repos/12/files?account=alice&ref=abc", http.StatusBadRequest, "bad_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, f.do(http.MethodGet, tt.path, h), tt.status, tt.code)
		})
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called for a rejected request")
	}
}

func TestInternalContentProviderErrors(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	h := internalHeader(internalToken)

	f.pulls.contentEr = provider.ErrNotFound
	for _, path := range []string{internalFiles, internalTreePath, internalFilePath} {
		assertError(t, f.do(http.MethodGet, path, h), http.StatusNotFound, "not_found")
	}
	f.pulls.contentEr = &statusErr{}
	for _, path := range []string{internalFiles, internalTreePath, internalFilePath} {
		assertError(t, f.do(http.MethodGet, path, h), http.StatusBadGateway, "upstream_failed")
	}
}

func TestInternalFilePathQueryIsDecoded(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	path := "/internal/v1/projects/7/repos/12/files?account=alice&ref=abc&path=" + url.QueryEscape("src/My File.java")

	assertOK(t, f.do(http.MethodGet, path, internalHeader(internalToken)))

	if f.pulls.gotPath != "src/My File.java" {
		t.Errorf("path = %q", f.pulls.gotPath)
	}
}
