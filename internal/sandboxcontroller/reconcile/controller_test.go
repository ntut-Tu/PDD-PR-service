package reconcile_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/reconcile"
	"pdd/services/internal/sandboxcontroller/runtime"
	"pdd/services/internal/sandboxcontroller/runtime/runtimetest"
	"pdd/services/internal/sandboxcontroller/store"
	"pdd/services/internal/sandboxcontroller/workspace"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type fakeSource struct {
	mu      sync.Mutex
	archive []byte
	err     error
	// block, when set, is waited on before answering.
	block chan struct{}
	calls int
}

func (s *fakeSource) Archive(ctx context.Context, _ model.Spec) (io.ReadCloser, error) {
	s.mu.Lock()
	s.calls++
	block, err, data := s.block, s.err, s.archive
	s.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "octo-hello-abc/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "octo-hello-abc/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

type harness struct {
	t       *testing.T
	ctx     context.Context
	runs    *store.Runs
	rt      *runtimetest.Fake
	src     *fakeSource
	ws      *workspace.Manager
	cache   string
	now     time.Time
	nowMu   sync.Mutex
	set     reconcile.Settings
	control *reconcile.Controller
}

func newHarness(t *testing.T, mutate ...func(*reconcile.Settings)) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &harness{
		t: t, ctx: context.Background(), runs: store.NewRuns(db), rt: runtimetest.New(),
		src:   &fakeSource{archive: tarball(t, map[string]string{"pom.xml": "<project/>"})},
		ws:    &workspace.Manager{Root: filepath.Join(dir, "ws"), MaxBytes: 1 << 20, MaxFiles: 1000},
		cache: filepath.Join(dir, "cache"), now: t0,
	}
	h.rt.Now = h.clock
	h.set = reconcile.Settings{
		MaxConcurrent: 2, Timeout: 5 * time.Minute, CPUs: 2, MemoryBytes: 2 << 30, Pids: 512, User: "1000:1000",
		NetworkName: "pdd-sandbox", ProxyPort: "3128", ImageMaven: "maven:test", ImageGradle: "gradle:test",
		CacheDir: h.cache, OutputMaxBytes: 4096, WorkspaceRetention: time.Hour,
	}
	for _, m := range mutate {
		m(&h.set)
	}
	h.control = h.newController()
	return h
}

func (h *harness) newController() *reconcile.Controller {
	return reconcile.New(reconcile.Deps{
		Store: h.runs, Runtime: h.rt, Workspaces: h.ws, Source: h.src,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: h.clock,
	}, h.set)
}

func (h *harness) clock() time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.nowMu.Lock()
	h.now = h.now.Add(d)
	h.nowMu.Unlock()
}

// create stores a Pending run.
func (h *harness) create(id string) {
	h.t.Helper()
	err := h.runs.Create(h.ctx, model.Run{ID: id, Spec: model.Spec{
		Account: "alice", ProjectID: 7, RepoID: 1, HeadSHA: strings.Repeat("a", 40),
		Files:       []model.File{{Path: "src/test/java/a/BTest.java", Content: "class BTest {}"}},
		TestClasses: []string{"a.BTest"},
	}, Status: model.Status{Phase: model.Pending, Message: "waiting", Conditions: []model.Condition{}, CreatedAt: h.clock()}})
	if err != nil {
		h.t.Fatal(err)
	}
}

// reconcile runs one pass and waits for the background preparation.
func (h *harness) reconcile() {
	h.t.Helper()
	if err := h.control.Reconcile(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	h.control.Wait()
}

func (h *harness) get(id string) model.Run {
	h.t.Helper()
	run, err := h.runs.Get(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return run
}

func (h *harness) phase(id string) model.Phase { return h.get(id).Status.Phase }

// setStatus rewrites the status of a stored run.
func (h *harness) setStatus(id string, mutate func(*model.Status)) {
	h.t.Helper()
	run := h.get(id)
	mutate(&run.Status)
	if err := h.runs.UpdateStatus(h.ctx, id, run.Status); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeReport(id, testdata string) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "report", "testdata", testdata))
	if err != nil {
		h.t.Fatal(err)
	}
	dir := filepath.Join(h.ws.SrcDir(id), "target", "surefire-reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "TEST-a.BTest.xml"), data, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) container(id string) runtime.Container {
	h.t.Helper()
	cts := h.rt.ForRun(id)
	if len(cts) != 1 {
		h.t.Fatalf("containers of %s = %v", id, cts)
	}
	return cts[0]
}

const id1, id2, id3 = "run_000000000000000000000001", "run_000000000000000000000002", "run_000000000000000000000003"

func TestPendingRunIsPreparedAndStarted(t *testing.T) {
	h := newHarness(t)
	h.create(id1)

	h.reconcile()

	run := h.get(id1)
	if run.Status.Phase != model.Running || run.Status.BuildTool != model.Maven || run.Status.StartedAt == nil || !run.Status.StartedAt.Equal(t0) {
		t.Fatalf("status = %+v", run.Status)
	}
	if len(run.Status.Conditions) != 2 || run.Status.Conditions[0].Type != "Prepared" || run.Status.Conditions[1].Type != "Started" {
		t.Errorf("conditions = %+v", run.Status.Conditions)
	}
	if got, _ := os.ReadFile(filepath.Join(h.ws.SrcDir(id1), "src/test/java/a/BTest.java")); string(got) != "class BTest {}" {
		t.Errorf("test file = %q", got)
	}
	if h.rt.Checked[0] != "runsc" {
		t.Errorf("checked = %v", h.rt.Checked)
	}
	spec := h.rt.Created[0]
	if spec.Runtime != "runsc" || spec.User != "1000:1000" || spec.Network != "pdd-sandbox" || spec.Image != "maven:test" ||
		spec.Labels[runtime.LabelRun] != id1 || spec.CPUs != 2 || spec.MemoryBytes != 2<<30 || spec.PidsLimit != 512 ||
		!spec.ReadonlyRootfs || strings.Join(spec.CapDrop, ",") != "ALL" || strings.Join(spec.SecurityOpt, ",") != "no-new-privileges" {
		t.Errorf("spec = %+v", spec)
	}
	if len(spec.Mounts) != 2 || spec.Mounts[0].Source != h.ws.Dir(id1) || spec.Mounts[0].Target != "/workspace" || spec.Mounts[0].ReadOnly ||
		spec.Mounts[1].Source != filepath.Join(h.cache, "7", "1") || spec.Mounts[1].Target != "/cache" {
		t.Errorf("mounts = %+v", spec.Mounts)
	}
	if !strings.Contains(strings.Join(spec.Cmd, " "), "-Dtest=a.BTest") || !strings.Contains(strings.Join(spec.Env, " "), "-Dhttps.proxyHost=172.30.0.1") {
		t.Errorf("cmd = %v env = %v", spec.Cmd, spec.Env)
	}
	if _, err := os.Stat(filepath.Join(h.ws.InitDir(id1), "settings.xml")); err != nil {
		t.Errorf("settings.xml: %v", err)
	}
}

func TestProxyHostPrefersConfiguredThenGateway(t *testing.T) {
	cases := []struct {
		name string
		set  string
		info runtime.NetworkInfo
		want string
	}{
		{"configured", "proxy.internal", runtime.NetworkInfo{Gateway: "10.0.0.1"}, "proxyHost=proxy.internal"},
		{"gateway", "", runtime.NetworkInfo{Gateway: "10.0.0.1"}, "proxyHost=10.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(s *reconcile.Settings) { s.ProxyHost = tc.set })
			h.rt.NetworkInfo = tc.info
			h.create(id1)

			h.reconcile()

			if env := strings.Join(h.rt.Created[0].Env, " "); !strings.Contains(env, tc.want) {
				t.Errorf("env = %s", env)
			}
		})
	}
}

func TestGradleProjectUsesGradleImage(t *testing.T) {
	h := newHarness(t)
	h.src.archive = tarball(t, map[string]string{"build.gradle.kts": "plugins {}"})
	h.create(id1)

	h.reconcile()

	if h.rt.Created[0].Image != "gradle:test" || h.get(id1).Status.BuildTool != model.Gradle {
		t.Errorf("spec = %+v status = %+v", h.rt.Created[0], h.get(id1).Status)
	}
}

func TestFinishedContainerBecomesSucceeded(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	ct := h.container(id1)
	h.writeReport(id1, "surefire-pass.xml")
	h.rt.Finish(ct.ID, 0, "[INFO] BUILD SUCCESS")
	h.advance(30 * time.Second)

	h.reconcile()

	run := h.get(id1)
	if run.Status.Phase != model.Succeeded || run.Status.Reason != "" || run.Status.FinishedAt == nil {
		t.Fatalf("status = %+v", run.Status)
	}
	res := run.Status.Result
	if !res.Compiled || len(res.Tests) != 1 || res.Tests[0].Passed != 3 || !strings.Contains(res.Output, "BUILD SUCCESS") {
		t.Errorf("result = %+v", res)
	}
	if h.rt.Count() != 0 {
		t.Error("container not removed")
	}
	if _, err := os.Stat(h.ws.Dir(id1)); err != nil {
		t.Error("workspace removed before retention")
	}
}

func TestFinishedContainerWithFailingTests(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	h.writeReport(id1, "surefire-mixed.xml")
	h.rt.Finish(h.container(id1).ID, 1, "Tests run: 4")

	h.reconcile()

	st := h.get(id1).Status
	if st.Phase != model.Failed || st.Reason != model.TestsFailed || st.Result.Tests[0].Failed != 1 || st.Result.Tests[0].Errors != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestFinishedContainerWithCompileError(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	log, _ := os.ReadFile(filepath.Join("..", "report", "testdata", "maven-compile-error.log"))
	h.rt.Finish(h.container(id1).ID, 1, string(log))

	h.reconcile()

	st := h.get(id1).Status
	if st.Phase != model.Failed || st.Reason != model.CompileFailed || st.Result.Compiled || !strings.Contains(st.Result.Output, "';' expected") {
		t.Errorf("status = %+v", st)
	}
}

func TestOutOfMemoryIsReported(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	h.rt.Finish(h.container(id1).ID, 137, "")
	h.rt.SetOOMKilled(h.container(id1).ID)

	h.reconcile()

	if st := h.get(id1).Status; st.Phase != model.Failed || !strings.Contains(st.Message, "out of memory") {
		t.Errorf("status = %+v", st)
	}
}

func TestRunningPastTimeoutIsStopped(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	ct := h.container(id1)
	h.advance(4 * time.Minute)
	h.reconcile()
	if h.phase(id1) != model.Running {
		t.Fatal("stopped before the timeout")
	}
	h.advance(2 * time.Minute)

	h.reconcile()

	st := h.get(id1).Status
	if st.Phase != model.Failed || st.Reason != model.Timeout || st.Result == nil || st.FinishedAt == nil {
		t.Fatalf("status = %+v", st)
	}
	if len(h.rt.Stopped) != 1 || h.rt.Stopped[0] != ct.ID || h.rt.Count() != 0 {
		t.Errorf("stopped %v, containers %d", h.rt.Stopped, h.rt.Count())
	}
}

func TestRunningWithMissingContainerIsError(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	_ = h.rt.Remove(h.ctx, h.container(id1).ID)

	h.reconcile()

	if st := h.get(id1).Status; st.Phase != model.Error || st.Reason != model.Internal {
		t.Errorf("status = %+v", st)
	}
}

func TestRunningWithNeverStartedContainerIsError(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.setStatus(id1, func(s *model.Status) { s.Phase = model.Running; s.StartedAt = &t0 })
	h.rt.Add(id1, runtime.StateCreated)

	h.reconcile()

	if st := h.get(id1).Status; st.Phase != model.Error || st.Reason != model.Internal {
		t.Errorf("status = %+v", st)
	}
}

func TestPreparingAfterRestart(t *testing.T) {
	cases := []struct {
		name  string
		state runtime.State
		none  bool
		want  model.Phase
	}{
		{"no container", "", true, model.Error},
		{"container created only", runtime.StateCreated, false, model.Running},
		{"container already running", runtime.StateRunning, false, model.Running},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.create(id1)
			h.setStatus(id1, func(s *model.Status) { s.Phase = model.Preparing })
			if !tc.none {
				h.rt.Add(id1, tc.state)
			}

			h.reconcile()

			st := h.get(id1).Status
			if st.Phase != tc.want {
				t.Fatalf("phase = %s", st.Phase)
			}
			if tc.want == model.Error && st.Reason != model.Internal {
				t.Errorf("reason = %s", st.Reason)
			}
			if tc.state == runtime.StateCreated && len(h.rt.Started) != 1 {
				t.Errorf("started = %v", h.rt.Started)
			}
			if tc.want == model.Running && st.StartedAt == nil {
				t.Error("startedAt missing")
			}
		})
	}
}

func TestPreparingStartFailureAfterRestart(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.setStatus(id1, func(s *model.Status) { s.Phase = model.Preparing })
	h.rt.Add(id1, runtime.StateCreated)
	h.rt.StartErr = runtimetest.ErrBoom

	h.reconcile()

	if st := h.get(id1).Status; st.Phase != model.Error || st.Reason != model.Internal {
		t.Errorf("status = %+v", st)
	}
}

func TestMaxConcurrentKeepsExtraRunsPending(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{id1, id2, id3} {
		h.create(id)
	}

	h.reconcile()

	if h.phase(id1) != model.Running || h.phase(id2) != model.Running || h.phase(id3) != model.Pending {
		t.Fatalf("phases = %s %s %s", h.phase(id1), h.phase(id2), h.phase(id3))
	}
	h.reconcile()
	if h.phase(id3) != model.Pending {
		t.Fatal("third run started above the limit")
	}

	h.writeReport(id1, "surefire-pass.xml")
	h.rt.Finish(h.container(id1).ID, 0, "")
	h.reconcile()

	if h.phase(id1) != model.Succeeded || h.phase(id3) != model.Running {
		t.Errorf("phases = %s %s", h.phase(id1), h.phase(id3))
	}
}

func TestMissingRunscEndsAsSandboxUnavailable(t *testing.T) {
	h := newHarness(t)
	h.rt.NoRunsc = true
	h.create(id1)

	h.reconcile()

	st := h.get(id1).Status
	if st.Phase != model.Error || st.Reason != model.SandboxUnavailable || !strings.Contains(st.Message, "sandbox unavailable") {
		t.Errorf("status = %+v", st)
	}
	if h.rt.Count() != 0 || h.src.calls != 0 {
		t.Error("work was done without runsc")
	}
	if h.control.SandboxAvailable(h.ctx) {
		t.Error("SandboxAvailable = true")
	}
}

func TestPreparationFailures(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(h *harness)
		reason model.Reason
		text   string
	}{
		{"source error", func(h *harness) { h.src.err = errors.New("gateway down") }, model.Internal, "gateway down"},
		{"unsupported project", func(h *harness) { h.src.archive = tarball(t, map[string]string{"package.json": "{}"}) }, model.UnsupportedProject, "not supported"},
		{"corrupt archive", func(h *harness) { h.src.archive = []byte("not a gzip") }, model.SourceUnavailable, "source unavailable"},
		{"network error", func(h *harness) { h.rt.NetworkErr = errors.New("no network") }, model.Internal, "no network"},
		{"network unavailable", func(h *harness) { h.rt.NetworkErr = runtime.ErrUnavailable }, model.SandboxUnavailable, "sandbox unavailable"},
		{"create error", func(h *harness) { h.rt.CreateErr = errors.New("no image") }, model.Internal, "no image"},
		{"start error", func(h *harness) { h.rt.StartErr = errors.New("cannot start") }, model.Internal, "cannot start"},
		{"cache dir unusable", func(h *harness) {
			if err := os.WriteFile(h.cache, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, model.Internal, "cache"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			h.create(id1)

			h.reconcile()

			st := h.get(id1).Status
			if st.Phase != model.Error || st.Reason != tc.reason || !strings.Contains(st.Message, tc.text) || st.FinishedAt == nil {
				t.Errorf("status = %+v", st)
			}
			if h.rt.Count() != 0 {
				t.Error("container left behind")
			}
		})
	}
}

func TestSourceUnavailableError(t *testing.T) {
	h := newHarness(t)
	h.src.err = errors.Join(errors.New("x"), sourceErr())
	h.create(id1)

	h.reconcile()

	if st := h.get(id1).Status; st.Reason != model.SourceUnavailable {
		t.Errorf("status = %+v", st)
	}
}

func TestNewTestFilesOutsideSrcTestAreRefused(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	run := h.get(id1)
	run.Spec.Files = []model.File{{Path: "pom.xml", Content: "x"}}
	// The API validates this; the controller must still refuse it when it reaches the workspace.
	_ = h.runs.Delete(h.ctx, id1)
	if err := h.runs.Create(h.ctx, run); err != nil {
		t.Fatal(err)
	}

	h.reconcile()

	if st := h.get(id1).Status; st.Phase != model.Error || st.Reason != model.Internal {
		t.Errorf("status = %+v", st)
	}
	if h.rt.Count() != 0 {
		t.Error("container created")
	}
}

func TestOrphansAreRemoved(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.setStatus(id1, func(s *model.Status) { s.Phase = model.Succeeded; s.FinishedAt = &t0 })
	h.rt.Add("run_ffffffffffffffffffffffff", runtime.StateRunning)
	h.rt.Add(id1, runtime.StateExited) // leftover of a finished run
	for _, name := range []string{"run_ffffffffffffffffffffffff", id1, "not-a-run"} {
		if err := os.MkdirAll(filepath.Join(h.ws.Root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	h.reconcile()

	if h.rt.Count() != 0 {
		t.Errorf("containers left: %d", h.rt.Count())
	}
	names, _ := h.ws.List()
	if strings.Join(names, ",") != "not-a-run,"+id1 {
		t.Errorf("workspaces = %v", names)
	}
}

func TestPendingRunWithStrayContainerLosesIt(t *testing.T) {
	h := newHarness(t, func(s *reconcile.Settings) { s.MaxConcurrent = 1 })
	h.create(id1)
	h.create(id2)
	h.setStatus(id1, func(s *model.Status) { s.Phase = model.Running; s.StartedAt = &t0 })
	h.rt.Add(id1, runtime.StateRunning)
	h.rt.Add(id2, runtime.StateRunning)

	h.reconcile()

	if len(h.rt.ForRun(id2)) != 0 || len(h.rt.ForRun(id1)) != 1 || h.phase(id2) != model.Pending {
		t.Errorf("containers of run 2: %v, phase %s", h.rt.ForRun(id2), h.phase(id2))
	}
}

func TestWorkspaceRetention(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	h.writeReport(id1, "surefire-pass.xml")
	h.rt.Finish(h.container(id1).ID, 0, "")
	h.reconcile()

	h.advance(59 * time.Minute)
	h.reconcile()
	if _, err := os.Stat(h.ws.Dir(id1)); err != nil {
		t.Fatal("workspace removed too early")
	}

	h.advance(2 * time.Minute)
	h.reconcile()

	if _, err := os.Stat(h.ws.Dir(id1)); err == nil {
		t.Error("workspace kept past retention")
	}
	if h.phase(id1) != model.Succeeded {
		t.Error("record changed")
	}
}

func TestDeletionRemovesContainerWorkspaceAndRecord(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	if err := h.runs.RequestDeletion(h.ctx, id1); err != nil {
		t.Fatal(err)
	}

	h.reconcile()

	if _, err := h.runs.Get(h.ctx, id1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("record: %v", err)
	}
	if h.rt.Count() != 0 {
		t.Error("container left")
	}
	if _, err := os.Stat(h.ws.Dir(id1)); err == nil {
		t.Error("workspace left")
	}
}

func TestDeletionOfPendingRun(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	_ = h.runs.RequestDeletion(h.ctx, id1)

	h.reconcile()

	if _, err := h.runs.Get(h.ctx, id1); !errors.Is(err, store.ErrNotFound) || h.src.calls != 0 {
		t.Errorf("err = %v calls = %d", err, h.src.calls)
	}
}

func TestDeletionCancelsPreparation(t *testing.T) {
	h := newHarness(t)
	h.src.block = make(chan struct{})
	h.create(id1)
	if err := h.control.Reconcile(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.phase(id1) != model.Preparing {
		t.Fatalf("phase = %s", h.phase(id1))
	}
	_ = h.runs.RequestDeletion(h.ctx, id1)

	if err := h.control.Reconcile(h.ctx); err != nil { // cancels the download
		t.Fatal(err)
	}
	h.control.Wait()
	h.reconcile() // removes the record

	if _, err := h.runs.Get(h.ctx, id1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("record: %v", err)
	}
	if h.rt.Count() != 0 {
		t.Error("container created for a deleted run")
	}
}

func TestDeletionRetriesWhenContainerCannotBeRemoved(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	_ = h.runs.RequestDeletion(h.ctx, id1)
	h.rt.RemoveErr = runtimetest.ErrBoom

	h.reconcile()
	if _, err := h.runs.Get(h.ctx, id1); err != nil {
		t.Fatal("record deleted although the container is still there")
	}

	h.rt.RemoveErr = nil
	h.reconcile()
	if _, err := h.runs.Get(h.ctx, id1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("record: %v", err)
	}
}

func TestRuntimeListFailureStillFailsPendingRunsButNothingElse(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	h.create(id2)
	h.rt.ListErr = runtimetest.ErrBoom
	h.rt.NoRunsc = true
	h.rt.Add("run_ffffffffffffffffffffffff", runtime.StateRunning)
	h.advance(time.Hour)

	h.reconcile()

	if h.phase(id1) != model.Running {
		t.Errorf("running run changed to %s", h.phase(id1))
	}
	if st := h.get(id2).Status; st.Reason != model.SandboxUnavailable {
		t.Errorf("pending run: %+v", st)
	}
	if len(h.rt.Removed) != 0 {
		t.Errorf("removed %v while the list failed", h.rt.Removed)
	}
	_ = h.runs.RequestDeletion(h.ctx, id1)
	h.reconcile()
	if _, err := h.runs.Get(h.ctx, id1); err != nil {
		t.Error("deleted without knowing the containers")
	}
}

func TestRestartedControllerFinishesRunsOfItsPredecessor(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	h.reconcile()
	h.writeReport(id1, "surefire-pass.xml")
	h.rt.Finish(h.container(id1).ID, 0, "ok")

	h.control = h.newController() // a new process with the same database and containers
	h.reconcile()

	if h.phase(id1) != model.Succeeded || h.rt.Count() != 0 {
		t.Errorf("phase %s containers %d", h.phase(id1), h.rt.Count())
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.create(id1)
	for i := 0; i < 3; i++ {
		h.reconcile()
	}
	if len(h.rt.Created) != 1 || h.phase(id1) != model.Running {
		t.Errorf("created %d, phase %s", len(h.rt.Created), h.phase(id1))
	}
}

func TestPreparationCancelledByShutdownLeavesRecordAlone(t *testing.T) {
	h := newHarness(t)
	h.src.block = make(chan struct{})
	h.create(id1)
	ctx, cancel := context.WithCancel(h.ctx)
	if err := h.control.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	h.control.Wait()

	if h.phase(id1) != model.Preparing {
		t.Errorf("phase = %s", h.phase(id1))
	}
	// The next process finds a Preparing run without container.
	h.control = h.newController()
	h.reconcile()
	if st := h.get(id1).Status; st.Phase != model.Error || st.Reason != model.Internal {
		t.Errorf("status = %+v", st)
	}
}

func TestRunLoopReactsToTriggerAndStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		h.control.Run(ctx, time.Hour)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)

	h.create(id1)
	h.control.Trigger()
	h.control.Trigger()

	deadline := time.Now().Add(5 * time.Second)
	for h.phase(id1) != model.Running {
		if time.Now().After(deadline) {
			t.Fatalf("run not started, phase %s", h.phase(id1))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestLongMessagesAreShortened(t *testing.T) {
	h := newHarness(t)
	h.src.err = errors.New(strings.Repeat("é line\n", 200))
	h.create(id1)

	h.reconcile()

	msg := h.get(id1).Status.Message
	if len(msg) > 310 || strings.Contains(msg, "\n") {
		t.Errorf("message has %d bytes: %q", len(msg), msg)
	}
}
