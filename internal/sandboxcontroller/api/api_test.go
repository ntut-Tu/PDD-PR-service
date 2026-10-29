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

	"pdd/services/internal/sandboxcontroller/api"
	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/store"
)

const token = "s3cret-token"

type fakeRuns struct {
	runs      map[string]model.Run
	createErr error
	getErr    error
	delErr    error
	pingErr   error
}

func (f *fakeRuns) Create(_ context.Context, run model.Run) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.runs[run.ID] = run
	return nil
}

func (f *fakeRuns) Get(_ context.Context, id string) (model.Run, error) {
	if f.getErr != nil {
		return model.Run{}, f.getErr
	}
	run, ok := f.runs[id]
	if !ok {
		return model.Run{}, store.ErrNotFound
	}
	return run, nil
}

func (f *fakeRuns) RequestDeletion(_ context.Context, id string) error {
	if f.delErr != nil {
		return f.delErr
	}
	run, ok := f.runs[id]
	if !ok {
		return store.ErrNotFound
	}
	run.DeletionRequested = true
	f.runs[id] = run
	return nil
}

func (f *fakeRuns) Ping(context.Context) error { return f.pingErr }

type env struct {
	runs     *fakeRuns
	triggers int
	runsc    bool
	handler  http.Handler
}

func newEnv() *env {
	e := &env{runs: &fakeRuns{runs: map[string]model.Run{}}, runsc: true}
	e.handler = api.NewHandler(api.Deps{
		InternalToken: token, Runs: e.runs, Trigger: func() { e.triggers++ },
		SandboxAvailable: func(context.Context) bool { return e.runsc },
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:              func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) },
	})
	return e
}

func (e *env) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func (e *env) auth(method, path, body string) *httptest.ResponseRecorder {
	return e.do(method, path, body, "X-Internal-Token", token)
}

const validBody = `{"spec":{"account":"alice","projectId":7,"repoId":1,"headSha":"0123456789abcdef0123456789abcdef01234567",
"files":[{"path":"src/test/java/a/BTest.java","content":"class BTest {}"}],"testClasses":["a.BTest"]}}`

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

func TestEndpointsRequireTheInternalToken(t *testing.T) {
	e := newEnv()
	id := model.NewID()
	e.runs.runs[id] = model.Run{ID: id}
	for _, tc := range []struct{ method, path string }{
		{"POST", "/internal/v1/runs"}, {"GET", "/internal/v1/runs/" + id}, {"DELETE", "/internal/v1/runs/" + id},
	} {
		for name, headers := range map[string][]string{
			"missing": nil, "wrong": {"X-Internal-Token", "nope"}, "prefix": {"X-Internal-Token", token[:5]},
			"longer": {"X-Internal-Token", token + "x"}, "bearer": {"Authorization", "Bearer " + token},
		} {
			rec := e.do(tc.method, tc.path, validBody, headers...)
			if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "unauthorized" {
				t.Errorf("%s %s (%s): %d %s", tc.method, tc.path, name, rec.Code, rec.Body)
			}
		}
	}
	if len(e.runs.runs) != 1 || e.runs.runs[id].DeletionRequested || e.triggers != 0 {
		t.Error("an unauthenticated request changed state")
	}
}

func TestCreateRun(t *testing.T) {
	e := newEnv()

	rec := e.auth("POST", "/internal/v1/runs", validBody)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var run model.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if !model.ValidID(run.ID) || run.Status.Phase != model.Pending || run.Spec.TestClasses[0] != "a.BTest" || run.DeletionRequested ||
		!run.Status.CreatedAt.Equal(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("run = %+v", run)
	}
	if _, ok := e.runs.runs[run.ID]; !ok || e.triggers != 1 {
		t.Errorf("stored %v, triggers %d", ok, e.triggers)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	status := raw["status"].(map[string]any)
	if _, ok := status["conditions"].([]any); !ok {
		t.Errorf("conditions must be a list: %v", status)
	}
	for _, key := range []string{"id", "spec", "status", "deletionRequested"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
}

func TestCreateRunRejectsBadRequests(t *testing.T) {
	cases := map[string]string{
		"not json":      `nope`,
		"empty":         ``,
		"no spec":       `{}`,
		"unknown field": `{"spec":{"account":"a"},"extra":1}`,
		"trailing":      validBody + `{}`,
		"bad sha":       strings.Replace(validBody, "0123456789abcdef0123456789abcdef01234567", "main", 1),
		"outside tests": strings.Replace(validBody, "src/test/java/a/BTest.java", "src/main/java/A.java", 1),
		"traversal":     strings.Replace(validBody, "src/test/java/a/BTest.java", "src/test/../../x", 1),
		"bad class":     strings.Replace(validBody, `"a.BTest"]`, `"a.B;rm"]`, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			rec := e.auth("POST", "/internal/v1/runs", body)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_request" {
				t.Errorf("status = %d: %s", rec.Code, rec.Body)
			}
			if len(e.runs.runs) != 0 || e.triggers != 0 {
				t.Error("state changed")
			}
		})
	}
}

func TestCreateRunBodyTooLarge(t *testing.T) {
	e := newEnv()
	body := `{"spec":{"account":"` + strings.Repeat("a", 9<<20) + `"}}`

	rec := e.auth("POST", "/internal/v1/runs", body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestCreateRunStoreFailure(t *testing.T) {
	e := newEnv()
	e.runs.createErr = errors.New("disk full")

	rec := e.auth("POST", "/internal/v1/runs", validBody)

	if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != "internal" || strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestCreateRunWithoutFilesStoresEmptyList(t *testing.T) {
	e := newEnv()
	body := `{"spec":{"account":"a","projectId":1,"repoId":1,"headSha":"0123456789abcdef0123456789abcdef01234567","testClasses":["a.B"]}}`

	rec := e.auth("POST", "/internal/v1/runs", body)

	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestGetRun(t *testing.T) {
	e := newEnv()
	id := model.NewID()
	e.runs.runs[id] = model.Run{ID: id, Status: model.Status{Phase: model.Running}}

	rec := e.auth("GET", "/internal/v1/runs/"+id, "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phase":"Running"`) {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestGetUnknownOrMalformedID(t *testing.T) {
	e := newEnv()
	for _, id := range []string{model.NewID(), "run_x", "..%2F..", "x"} {
		rec := e.auth("GET", "/internal/v1/runs/"+id, "")
		if rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
			t.Errorf("%s: %d %s", id, rec.Code, rec.Body)
		}
	}
	e.runs.getErr = errors.New("db down")
	rec := e.auth("GET", "/internal/v1/runs/"+model.NewID(), "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: %d", rec.Code)
	}
}

func TestDeleteRunIsAcceptedAndAsynchronous(t *testing.T) {
	e := newEnv()
	id := model.NewID()
	e.runs.runs[id] = model.Run{ID: id}

	rec := e.auth("DELETE", "/internal/v1/runs/"+id, "")

	if rec.Code != http.StatusAccepted || !e.runs.runs[id].DeletionRequested || e.triggers != 1 {
		t.Errorf("status = %d, run = %+v, triggers = %d", rec.Code, e.runs.runs[id], e.triggers)
	}
	if again := e.auth("DELETE", "/internal/v1/runs/"+id, ""); again.Code != http.StatusAccepted {
		t.Errorf("second delete = %d", again.Code)
	}
}

func TestDeleteErrors(t *testing.T) {
	e := newEnv()
	if rec := e.auth("DELETE", "/internal/v1/runs/"+model.NewID(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown: %d", rec.Code)
	}
	id := model.NewID()
	e.runs.runs[id] = model.Run{ID: id}
	e.runs.delErr = errors.New("db down")
	if rec := e.auth("DELETE", "/internal/v1/runs/"+id, ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("store error: %d", rec.Code)
	}
	e.runs.delErr = store.ErrNotFound // deleted between the read and the update
	if rec := e.auth("DELETE", "/internal/v1/runs/"+id, ""); rec.Code != http.StatusAccepted {
		t.Errorf("race: %d", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv()
	rec := e.do("GET", "/healthz", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"runsc":true}` {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}
	e.runsc = false
	rec = e.do("GET", "/healthz", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"runsc":false}` {
		t.Errorf("without runsc: %d: %s", rec.Code, rec.Body)
	}
	e.runs.pingErr = errors.New("db down")
	if rec := e.do("GET", "/healthz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("db down: %d", rec.Code)
	}
}

func TestUnknownEndpointAndMethod(t *testing.T) {
	e := newEnv()
	if rec := e.auth("GET", "/internal/v1/other", ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found" {
		t.Errorf("unknown: %d %s", rec.Code, rec.Body)
	}
	if rec := e.auth("PUT", "/internal/v1/runs", validBody); rec.Code != http.StatusNotFound {
		t.Errorf("method: %d", rec.Code)
	}
}

func TestPanicsBecome500(t *testing.T) {
	e := newEnv()
	e.runs.runs = nil // writing to a nil map panics inside the handler
	rec := e.auth("POST", "/internal/v1/runs", validBody)
	if rec.Code != http.StatusInternalServerError || errorCode(t, rec) != "internal" {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestRequestLogDoesNotContainTokenOrQuery(t *testing.T) {
	var logs bytes.Buffer
	h := api.NewHandler(api.Deps{
		InternalToken: token, Runs: newEnv().runs, Trigger: func() {},
		SandboxAvailable: func(context.Context) bool { return true },
		Logger:           slog.New(slog.NewTextHandler(&logs, nil)),
	})
	req := httptest.NewRequest("GET", "/internal/v1/runs/"+model.NewID()+"?secret=1", nil)
	req.Header.Set("X-Internal-Token", token)

	h.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "secret=1") || !strings.Contains(logs.String(), "status=404") {
		t.Errorf("log = %s", logs.String())
	}
}
