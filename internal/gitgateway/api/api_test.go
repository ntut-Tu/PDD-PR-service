package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/api"
	"pdd/services/internal/gitgateway/auth"
	"pdd/services/internal/gitgateway/auth/authtest"
	"pdd/services/internal/gitgateway/backend"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/service"
	"pdd/services/internal/gitgateway/store"
	"pdd/services/pkg/gitprovider"
)

const (
	secret        = "test-secret"
	internalToken = "internal-secret"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type fakeBackend struct {
	projects map[int64]bool
	repos    []model.Repo
	err      error
	reposJWT string
}

func (f *fakeBackend) IsMember(_ context.Context, _, _ string, projectID int64) (bool, error) {
	return f.projects[projectID], f.err
}

func (f *fakeBackend) Repos(_ context.Context, jwt string, _ int64) ([]model.Repo, error) {
	f.reposJWT = jwt
	return f.repos, f.err
}

type fakeTokens struct {
	saved map[string]string
	err   error
}

func (f *fakeTokens) Save(_ context.Context, account, token string) error {
	if f.err != nil {
		return f.err
	}
	f.saved[account] = token
	return nil
}

func (f *fakeTokens) Load(_ context.Context, account string) (string, error) {
	token, ok := f.saved[account]
	if !ok {
		return "", store.ErrTokenNotFound
	}
	return token, nil
}

type fakePulls struct {
	list      service.ListResult
	gotRepos  []model.Repo
	detail    service.DetailResult
	detailErr error
	gotToken  string
	gotRepo   model.Repo
	gotNumber int

	files     service.FilesResult
	tree      service.TreeResult
	file      service.FileResult
	contentEr error
	gotRef    string
	gotPath   string
}

func (f *fakePulls) PullFiles(_ context.Context, token string, repo model.Repo, number int) (service.FilesResult, error) {
	f.gotToken, f.gotRepo, f.gotNumber = token, repo, number
	return f.files, f.contentEr
}

func (f *fakePulls) PullTree(_ context.Context, token string, repo model.Repo, number int) (service.TreeResult, error) {
	f.gotToken, f.gotRepo, f.gotNumber = token, repo, number
	return f.tree, f.contentEr
}

func (f *fakePulls) Tree(_ context.Context, token string, repo model.Repo, ref string) (service.TreeResult, error) {
	f.gotToken, f.gotRepo, f.gotRef = token, repo, ref
	return f.tree, f.contentEr
}

func (f *fakePulls) File(_ context.Context, token string, repo model.Repo, ref, path string) (service.FileResult, error) {
	f.gotToken, f.gotRepo, f.gotRef, f.gotPath = token, repo, ref, path
	return f.file, f.contentEr
}

func (f *fakePulls) ListPulls(_ context.Context, token string, repos []model.Repo) service.ListResult {
	f.gotToken, f.gotRepos = token, repos
	return f.list
}

func (f *fakePulls) PullDetail(_ context.Context, token string, repo model.Repo, number int) (service.DetailResult, error) {
	f.gotToken, f.gotRepo, f.gotNumber = token, repo, number
	return f.detail, f.detailErr
}

type fixture struct {
	backend *fakeBackend
	tokens  *fakeTokens
	pulls   *fakePulls
	logs    *bytes.Buffer
	handler http.Handler
	health  error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		backend: &fakeBackend{
			projects: map[int64]bool{7: true},
			repos: []model.Repo{
				{ID: 12, Provider: model.GitHub, Owner: "o", Name: "n"},
				{ID: 13, Provider: model.GitLab, BaseURL: "https://gl", Owner: "g", Name: "m"},
			},
		},
		tokens: &fakeTokens{saved: map[string]string{}},
		pulls:  &fakePulls{list: service.ListResult{Pulls: []model.Pull{}, Errors: []service.RepoError{}, FetchedAt: now}},
		logs:   &bytes.Buffer{},
	}
	f.handler = api.NewHandler(api.Deps{
		Verifier:      auth.NewVerifier(secret, func() time.Time { return now }),
		InternalToken: internalToken,
		Backend:       f.backend,
		Tokens:        f.tokens,
		Pulls:         f.pulls,
		Health:        func(context.Context) error { return f.health },
		Logger:        slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	return f
}

func (f *fixture) do(method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for name, values := range header {
		req.Header[name] = values
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func bearer(account, oauth string, expires time.Time) http.Header {
	return http.Header{"Authorization": {"Bearer " + authtest.Sign(secret, account, oauth, expires)}}
}

func validAuth() http.Header { return bearer("alice", "gho_alice", now.Add(10*time.Minute)) }

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	return v
}

type errorBody struct {
	Error struct{ Code, Message string } `json:"error"`
}

func assertError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body)
	}
	if got := decode[errorBody](t, rec).Error.Code; got != code {
		t.Fatalf("error code = %q, want %q", got, code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func assertOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
}

func TestListPullsSuccess(t *testing.T) {
	f := newFixture(t)
	f.pulls.list = service.ListResult{
		Pulls:     []model.Pull{{RepoID: 12, Provider: model.GitHub, Number: 42, Title: "T", State: "open", Mergeable: model.MergeableYes, UpdatedAt: now}},
		FetchedAt: now,
		Errors:    []service.RepoError{{RepoID: 13, Repo: "o/m", Code: "upstream_failed", Message: "x"}},
	}

	rec := f.do(http.MethodGet, "/api/v1/projects/7/pulls", validAuth())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	raw := decode[map[string]any](t, rec)
	for _, field := range []string{"pulls", "stale", "fetchedAt", "errors"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("response lacks %q: %s", field, rec.Body)
		}
	}
	pulls := raw["pulls"].([]any)
	pull := pulls[0].(map[string]any)
	for _, field := range []string{"repoId", "provider", "repo", "number", "title", "state", "draft", "mergeable", "author", "sourceBranch", "targetBranch", "url", "updatedAt"} {
		if _, ok := pull[field]; !ok {
			t.Errorf("pull lacks %q: %v", field, pull)
		}
	}
	if raw["fetchedAt"] != "2026-10-24T12:00:00Z" || raw["stale"] != false {
		t.Errorf("fetchedAt/stale = %v / %v", raw["fetchedAt"], raw["stale"])
	}
	errs := raw["errors"].([]any)[0].(map[string]any)
	if errs["repoId"] != float64(13) || errs["repo"] != "o/m" || errs["code"] != "upstream_failed" {
		t.Errorf("errors[0] = %v", errs)
	}
	if f.pulls.gotToken != "gho_alice" {
		t.Errorf("provider token = %q, want the oauth claim", f.pulls.gotToken)
	}
	if f.backend.reposJWT == "" {
		t.Error("caller JWT was not forwarded to the backend")
	}
}

func TestListRepoPullsRequestsOnlyThatRepository(t *testing.T) {
	f := newFixture(t)
	f.pulls.list = service.ListResult{
		Pulls:     []model.Pull{{RepoID: 13, Provider: model.GitLab, Number: 3, HeadSHA: strings.Repeat("c", 40), UpdatedAt: now}},
		FetchedAt: now,
		Errors:    []service.RepoError{},
	}

	rec := f.do(http.MethodGet, "/api/v1/projects/7/repos/13/pulls", validAuth())

	assertOK(t, rec)
	raw := decode[map[string]any](t, rec)
	for _, field := range []string{"pulls", "stale", "fetchedAt", "errors"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("response lacks %q: %s", field, rec.Body)
		}
	}
	if pull := raw["pulls"].([]any)[0].(map[string]any); pull["headSha"] != strings.Repeat("c", 40) || pull["repoId"] != float64(13) {
		t.Errorf("pull = %v", pull)
	}
	if len(f.pulls.gotRepos) != 1 || f.pulls.gotRepos[0].ID != 13 {
		t.Errorf("repositories requested from the provider = %+v, want only repo 13", f.pulls.gotRepos)
	}
	if f.pulls.gotToken != "gho_alice" {
		t.Errorf("provider token = %q, want the oauth claim", f.pulls.gotToken)
	}
}

func TestListRepoPullsForwardsStaleAndErrors(t *testing.T) {
	f := newFixture(t)
	f.pulls.list = service.ListResult{
		Pulls: []model.Pull{}, Stale: true, FetchedAt: now.Add(-time.Hour),
		Errors: []service.RepoError{{RepoID: 12, Repo: "o/n", Code: "quota_reserve", Message: "x"}},
	}

	rec := f.do(http.MethodGet, "/api/v1/projects/7/repos/12/pulls", validAuth())

	assertOK(t, rec)
	body := decode[map[string]any](t, rec)
	if body["stale"] != true || body["fetchedAt"] != "2026-10-24T11:00:00Z" || len(body["errors"].([]any)) != 1 {
		t.Errorf("body = %v", body)
	}
}

func TestListRepoPullsNotFound(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{
		"/api/v1/projects/7/repos/99/pulls",
		"/api/v1/projects/7/repos/abc/pulls",
		"/api/v1/projects/abc/repos/12/pulls",
	} {
		assertError(t, f.do(http.MethodGet, path, validAuth()), http.StatusNotFound, "not_found")
	}
	if f.pulls.gotRepos != nil {
		t.Error("provider called for an unknown repository")
	}
}

func TestListRepoPullsRejectsRepositoryOfAnotherKind(t *testing.T) {
	f := newFixture(t)
	f.backend.repos = []model.Repo{{ID: 12, Provider: model.GitHub, Owner: "o", Name: "n"}}

	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/repos/14/pulls", validAuth()), http.StatusNotFound, "not_found")
}

func TestListPullsStaleAndEmpty(t *testing.T) {
	f := newFixture(t)
	f.pulls.list = service.ListResult{Pulls: []model.Pull{}, Errors: []service.RepoError{}, Stale: true, FetchedAt: now.Add(-time.Hour)}

	rec := f.do(http.MethodGet, "/api/v1/projects/7/pulls", validAuth())

	body := decode[map[string]any](t, rec)
	if body["stale"] != true || body["fetchedAt"] != "2026-10-24T11:00:00Z" {
		t.Errorf("body = %v", body)
	}
	if !strings.Contains(rec.Body.String(), `"pulls":[]`) || !strings.Contains(rec.Body.String(), `"errors":[]`) {
		t.Errorf("empty collections must encode as []: %s", rec.Body)
	}
}

func TestAuthentication(t *testing.T) {
	paths := []string{"/api/v1/projects/7/pulls", "/api/v1/projects/7/repos/12/pulls", "/api/v1/projects/7/repos/12/pulls/42"}
	tests := map[string]http.Header{
		"missing header": nil,
		"not bearer":     {"Authorization": {"Basic abc"}},
		"garbage":        {"Authorization": {"Bearer garbage"}},
		"bad signature":  {"Authorization": {"Bearer " + authtest.Sign("other", "alice", "gho", now.Add(time.Minute))}},
		"expired":        bearer("alice", "gho", now.Add(-time.Second)),
		"missing oauth":  bearer("alice", "", now.Add(time.Minute)),
	}
	for _, path := range paths {
		for name, header := range tests {
			t.Run(name+" "+path, func(t *testing.T) {
				f := newFixture(t)

				rec := f.do(http.MethodGet, path, header)

				assertError(t, rec, http.StatusUnauthorized, "unauthorized")
				if len(f.tokens.saved) != 0 || f.pulls.gotToken != "" {
					t.Error("a rejected request stored a token or called the provider")
				}
			})
		}
	}
}

func TestAuthenticatedRequestPersistsProviderToken(t *testing.T) {
	f := newFixture(t)

	f.do(http.MethodGet, "/api/v1/projects/7/pulls", validAuth())

	if f.tokens.saved["alice"] != "gho_alice" {
		t.Fatalf("saved tokens = %v", f.tokens.saved)
	}
}

func TestPersistFailureDoesNotFailRequestAndTokenIsNotLogged(t *testing.T) {
	f := newFixture(t)
	f.tokens.err = errors.New("disk full")

	rec := f.do(http.MethodGet, "/api/v1/projects/7/pulls", validAuth())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "disk full") {
		t.Errorf("failure not logged: %s", logs)
	}
	if strings.Contains(logs, "gho_alice") || strings.Contains(logs, "Bearer") {
		t.Errorf("logs leak credentials: %s", logs)
	}
}

func TestForbiddenForNonMember(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/api/v1/projects/8/pulls", "/api/v1/projects/8/repos/12/pulls", "/api/v1/projects/8/repos/12/pulls/1"} {
		assertError(t, f.do(http.MethodGet, path, validAuth()), http.StatusForbidden, "forbidden")
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called for a non-member")
	}
}

func TestBackendFailureIsBadGateway(t *testing.T) {
	f := newFixture(t)
	f.backend.err = backend.ErrUnavailable

	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/pulls", validAuth()), http.StatusBadGateway, "upstream_failed")
	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/repos/12/pulls", validAuth()), http.StatusBadGateway, "upstream_failed")
	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/repos/12/pulls/1", validAuth()), http.StatusBadGateway, "upstream_failed")
}

func TestDetailSuccess(t *testing.T) {
	f := newFixture(t)
	f.pulls.detail = service.DetailResult{
		Pull:      model.PullDetail{Pull: model.Pull{Number: 42}, Description: "desc", Commits: []model.Commit{}, Comments: []model.Comment{}},
		Stale:     true,
		FetchedAt: now,
	}

	rec := f.do(http.MethodGet, "/api/v1/projects/7/repos/12/pulls/42", validAuth())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	body := decode[map[string]any](t, rec)
	pull := body["pull"].(map[string]any)
	if body["stale"] != true || body["fetchedAt"] != "2026-10-24T12:00:00Z" || pull["description"] != "desc" || pull["number"] != float64(42) {
		t.Errorf("body = %v", body)
	}
	if _, ok := pull["commits"]; !ok {
		t.Errorf("pull lacks commits: %v", pull)
	}
	if f.pulls.gotRepo.ID != 12 || f.pulls.gotNumber != 42 || f.pulls.gotToken != "gho_alice" {
		t.Errorf("provider call = repo %+v number %d token %q", f.pulls.gotRepo, f.pulls.gotNumber, f.pulls.gotToken)
	}
}

func TestDetailNotFound(t *testing.T) {
	paths := []string{
		"/api/v1/projects/7/repos/99/pulls/1", // repo not bound to the project
		"/api/v1/projects/7/repos/12/pulls/x",
		"/api/v1/projects/7/repos/x/pulls/1",
		"/api/v1/projects/x/pulls",
		"/api/v1/projects/x/repos/12/pulls/1",
		"/api/v1/nothing",
	}
	for _, path := range paths {
		assertError(t, newFixture(t).do(http.MethodGet, path, validAuth()), http.StatusNotFound, "not_found")
	}

	f := newFixture(t)
	f.pulls.detailErr = provider.ErrNotFound
	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/repos/12/pulls/5", validAuth()), http.StatusNotFound, "not_found")
}

func TestDetailProviderErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"quota reserve", &gitprovider.QuotaReserveError{Remaining: 1, Reserve: 10, ResetAt: now.Add(time.Hour)}, http.StatusServiceUnavailable, "quota_reserve"},
		{"max retry", &gitprovider.MaxRetryError{Attempts: 3, Err: errors.New("503")}, http.StatusBadGateway, "upstream_failed"},
		{"other status", &gitprovider.StatusError{StatusCode: 400}, http.StatusBadGateway, "upstream_failed"},
		{"provider unauthorized", &gitprovider.StatusError{StatusCode: 401}, http.StatusBadGateway, "provider_unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.pulls.detailErr = tt.err

			assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/repos/12/pulls/1", validAuth()), tt.status, tt.code)
		})
	}
}

func internalHeader(token string) http.Header { return http.Header{"X-Internal-Token": {token}} }

func TestInternalAPIUsesStoredTokenAfterJWTExpired(t *testing.T) {
	f := newFixture(t)
	f.pulls.detail = service.DetailResult{Pull: model.PullDetail{Description: "d"}, FetchedAt: now}

	// The user calls once, then the JWT expires.
	assertOK(t, f.do(http.MethodGet, "/api/v1/projects/7/pulls", bearer("alice", "gho_alice", now.Add(time.Minute))))
	expired := bearer("alice", "gho_alice", now.Add(-time.Hour))
	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/pulls", expired), http.StatusUnauthorized, "unauthorized")

	rec := f.do(http.MethodGet, "/internal/v1/projects/7/repos/12/pulls/42?account=alice", internalHeader(internalToken))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if f.pulls.gotToken != "gho_alice" || f.pulls.gotNumber != 42 {
		t.Errorf("provider call token %q number %d", f.pulls.gotToken, f.pulls.gotNumber)
	}
	if got := decode[map[string]any](t, rec); got["pull"].(map[string]any)["description"] != "d" {
		t.Errorf("body = %v", got)
	}
	if f.backend.reposJWT != "" {
		t.Errorf("internal call forwarded a JWT: %q", f.backend.reposJWT)
	}
}

func TestInternalAPIRequiresToken(t *testing.T) {
	f := newFixture(t)
	f.tokens.saved["alice"] = "gho_alice"
	path := "/internal/v1/projects/7/repos/12/pulls/42?account=alice"

	for name, header := range map[string]http.Header{
		"missing":      nil,
		"wrong":        internalHeader("nope"),
		"empty":        internalHeader(""),
		"prefix":       internalHeader(internalToken[:5]),
		"longer":       internalHeader(internalToken + "x"),
		"bearer jwt":   validAuth(),
		"jwt instead":  {"X-Internal-Token": {authtest.Sign(secret, "alice", "x", now.Add(time.Minute))}},
		"wrong header": {"Internal-Token": {internalToken}},
	} {
		t.Run(name, func(t *testing.T) {
			assertError(t, f.do(http.MethodGet, path, header), http.StatusUnauthorized, "unauthorized")
		})
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called without a valid internal token")
	}
}

func TestInternalAPIErrors(t *testing.T) {
	f := newFixture(t)
	f.tokens.saved["alice"] = "gho_alice"
	h := internalHeader(internalToken)

	assertError(t, f.do(http.MethodGet, "/internal/v1/projects/7/repos/12/pulls/42", h), http.StatusBadRequest, "bad_request")
	assertError(t, f.do(http.MethodGet, "/internal/v1/projects/7/repos/12/pulls/42?account=nobody", h), http.StatusNotFound, "not_found")
	assertError(t, f.do(http.MethodGet, "/internal/v1/projects/x/repos/12/pulls/42?account=alice", h), http.StatusNotFound, "not_found")
	assertError(t, f.do(http.MethodGet, "/internal/v1/projects/7/repos/99/pulls/42?account=alice", h), http.StatusNotFound, "not_found")

	f.pulls.detailErr = &gitprovider.QuotaReserveError{}
	assertError(t, f.do(http.MethodGet, "/internal/v1/projects/7/repos/12/pulls/42?account=alice", h), http.StatusServiceUnavailable, "quota_reserve")

	f.backend.err = backend.ErrUnavailable
	assertError(t, f.do(http.MethodGet, "/internal/v1/projects/7/repos/12/pulls/42?account=alice", h), http.StatusBadGateway, "upstream_failed")
}

type brokenTokens struct{ fakeTokens }

func (brokenTokens) Load(context.Context, string) (string, error) { return "", errors.New("db down") }

func TestInternalAPITokenStoreFailure(t *testing.T) {
	f := newFixture(t)
	handler := api.NewHandler(api.Deps{
		Verifier: auth.NewVerifier(secret, nil), InternalToken: internalToken, Backend: f.backend,
		Tokens: &brokenTokens{}, Pulls: f.pulls, Health: func(context.Context) error { return nil },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/projects/7/repos/12/pulls/1?account=alice", nil)
	req.Header.Set("X-Internal-Token", internalToken)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusInternalServerError, "internal")
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	rec := f.do(http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK || decode[map[string]string](t, rec)["status"] != "ok" {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body)
	}

	f.health = errors.New("db closed")
	assertError(t, f.do(http.MethodGet, "/healthz", nil), http.StatusServiceUnavailable, "unavailable")
}

type panicPulls struct{ fakePulls }

func (*panicPulls) ListPulls(context.Context, string, []model.Repo) service.ListResult { panic("boom") }

func TestPanicIsRecoveredAndRequestsAreLogged(t *testing.T) {
	f := newFixture(t)
	handler := api.NewHandler(api.Deps{
		Verifier: auth.NewVerifier(secret, func() time.Time { return now }), InternalToken: internalToken, Backend: f.backend,
		Tokens: f.tokens, Pulls: &panicPulls{}, Health: func(context.Context) error { return nil },
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/7/pulls?secret=q", nil)
	req.Header = validAuth()
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assertError(t, rec, http.StatusInternalServerError, "internal")
	logs := f.logs.String()
	if !strings.Contains(logs, "panic in handler") || !strings.Contains(logs, "status=500") || !strings.Contains(logs, "path=/api/v1/projects/7/pulls") {
		t.Errorf("logs = %s", logs)
	}
	if strings.Contains(logs, "secret=q") || strings.Contains(logs, "gho_alice") {
		t.Errorf("logs leak the query or token: %s", logs)
	}
}
