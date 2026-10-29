package app_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/app"
	"pdd/services/internal/sandboxcontroller/config"
	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/runtime"
	"pdd/services/internal/sandboxcontroller/runtime/runtimetest"
)

const token = "integration-token"

// gateway is a fake git-gateway serving one Maven project as a GitHub-style tarball.
type gateway struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	tokens   []string
	status   int
}

func newGateway(t *testing.T, files map[string]string) *gateway {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"}})
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "octo-hello-0123456/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	g := &gateway{status: http.StatusOK}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.requests = append(g.requests, r.URL.Path+"?"+r.URL.RawQuery)
		g.tokens = append(g.tokens, r.Header.Get("X-Internal-Token"))
		status := g.status
		g.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"code":"upstream_failed","message":"provider unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(g.Close)
	return g
}

type service struct {
	t       *testing.T
	app     *app.App
	cancel  context.CancelFunc
	done    chan error
	baseURL string
}

func configFor(t *testing.T, dir, gatewayURL, extra string) config.Config {
	t.Helper()
	cfg, err := config.LoadYAML([]byte(fmt.Sprintf(`
security: {internalToken: %s}
server: {listenAddr: "127.0.0.1:0", shutdownTimeout: 2s}
gitGateway: {url: %q, internalToken: %s}
storage: {dataDir: %q}
workspace: {dir: %q}
cache: {dir: %q}
network: {allowedHosts: [repo.maven.apache.org], proxy: {listen: "127.0.0.1:0"}}
reconcile: {resyncInterval: 30ms}
retention: {workspace: 1h}
`, token, gatewayURL, token, filepath.Join(dir, "data"), filepath.Join(dir, "ws"), filepath.Join(dir, "cache"))), []byte(extra))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func start(t *testing.T, cfg config.Config, rt runtime.Runtime, opts ...app.Option) *service {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.New(context.Background(), cfg, log, append([]app.Option{app.WithRuntime(rt)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &service{t: t, app: a, cancel: cancel, done: make(chan error, 1), baseURL: "http://" + a.APIAddr()}
	go func() { s.done <- a.Run(ctx) }()
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *service) stop() {
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Errorf("Run: %v", err)
		}
		s.done <- nil
		_ = s.app.Close()
	case <-time.After(10 * time.Second):
		s.t.Error("service did not stop")
	}
}

func (s *service) request(method, path, body string, auth bool) (int, []byte) {
	s.t.Helper()
	req, _ := http.NewRequest(method, s.baseURL+path, strings.NewReader(body))
	if auth {
		req.Header.Set("X-Internal-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (s *service) create(classes ...string) string {
	s.t.Helper()
	spec := map[string]any{"spec": map[string]any{
		"account": "alice", "projectId": 7, "repoId": 1, "headSha": strings.Repeat("a", 40),
		"files":       []map[string]string{{"path": "src/test/java/a/BTest.java", "content": "package a; class BTest {}"}},
		"testClasses": classes,
	}}
	data, _ := json.Marshal(spec)
	status, body := s.request("POST", "/internal/v1/runs", string(data), true)
	if status != http.StatusCreated {
		s.t.Fatalf("create: %d %s", status, body)
	}
	var run model.Run
	_ = json.Unmarshal(body, &run)
	return run.ID
}

func (s *service) get(id string) (int, model.Run) {
	s.t.Helper()
	status, body := s.request("GET", "/internal/v1/runs/"+id, "", true)
	var run model.Run
	_ = json.Unmarshal(body, &run)
	return status, run
}

func (s *service) waitFor(id string, ok func(model.Run) bool) model.Run {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, run := s.get(id)
		if ok(run) {
			return run
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out; status = %+v", run.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func terminal(r model.Run) bool { return r.Status.Phase.Terminal() }

func phaseIs(p model.Phase) func(model.Run) bool {
	return func(r model.Run) bool { return r.Status.Phase == p }
}

var mavenProject = map[string]string{"pom.xml": "<project/>", "src/main/java/a/B.java": "package a; public class B {}"}

// simulate plays Maven: it writes a surefire report into the mounted workspace and exits.
func simulate(rt *runtimetest.Fake, report string, exit int, logs string) func(runtime.ContainerSpec, string) {
	return func(spec runtime.ContainerSpec, id string) {
		data, err := os.ReadFile(filepath.Join("..", "report", "testdata", report))
		if err == nil {
			dir := filepath.Join(spec.Mounts[0].Source, "src", "target", "surefire-reports")
			_ = os.MkdirAll(dir, 0o755)
			_ = os.WriteFile(filepath.Join(dir, "TEST-a.BTest.xml"), data, 0o644)
		}
		rt.Finish(id, exit, logs)
	}
}

func TestRunSucceedsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	rt.OnStart = simulate(rt, "surefire-pass.xml", 0, "[INFO] BUILD SUCCESS")
	s := start(t, configFor(t, dir, gw.URL, ""), rt)

	id := s.create("a.BTest")
	run := s.waitFor(id, terminal)

	if run.Status.Phase != model.Succeeded || run.Status.BuildTool != model.Maven || run.Status.Result == nil || !run.Status.Result.Compiled {
		t.Fatalf("status = %+v", run.Status)
	}
	if tests := run.Status.Result.Tests; len(tests) != 1 || tests[0].Class != "a.BTest" || tests[0].Passed != 3 {
		t.Errorf("tests = %+v", tests)
	}
	if run.Status.StartedAt == nil || run.Status.FinishedAt == nil || run.Spec.Files[0].Content != "package a; class BTest {}" {
		t.Errorf("run = %+v", run)
	}
	waitUntil(t, func() bool { return rt.Count() == 0 })
	if len(gw.requests) != 1 || !strings.HasPrefix(gw.requests[0], "/internal/v1/projects/7/repos/1/archive?account=alice&ref=") || gw.tokens[0] != token {
		t.Errorf("gateway saw %v %v", gw.requests, gw.tokens)
	}
	spec := rt.Created[0]
	if spec.Runtime != "runsc" || !strings.Contains(strings.Join(spec.Env, " "), "-Dhttps.proxyPort="+portOf(s.app.ProxyAddr())) {
		t.Errorf("container spec = %+v", spec)
	}
	if spec.Mounts[1].Source != filepath.Join(dir, "cache", "7", "1") {
		t.Errorf("cache mount = %v", spec.Mounts[1])
	}
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEachTryIsItsOwnRunAndFailuresAreKept(t *testing.T) {
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	rt.OnStart = simulate(rt, "surefire-mixed.xml", 1, "Tests run: 4, Failures: 1, Errors: 1")
	s := start(t, configFor(t, t.TempDir(), gw.URL, ""), rt)

	first := s.waitFor(s.create("a.BTest"), terminal)
	rt.OnStart = simulate(rt, "surefire-pass.xml", 0, "ok")
	second := s.waitFor(s.create("a.BTest"), terminal)

	if first.Status.Phase != model.Failed || first.Status.Reason != model.TestsFailed || first.Status.Result.Tests[0].Failed != 1 {
		t.Errorf("first = %+v", first.Status)
	}
	if second.Status.Phase != model.Succeeded || first.ID == second.ID {
		t.Errorf("second = %+v", second.Status)
	}
	if _, again := s.get(first.ID); again.Status.Phase != model.Failed {
		t.Error("the first try was not kept")
	}
}

func TestCompileFailureKeepsErrorLines(t *testing.T) {
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	logs, _ := os.ReadFile(filepath.Join("..", "report", "testdata", "maven-compile-error.log"))
	rt.OnStart = func(_ runtime.ContainerSpec, id string) { rt.Finish(id, 1, string(logs)) }
	s := start(t, configFor(t, t.TempDir(), gw.URL, "output: {maxBytes: 600}"), rt)

	run := s.waitFor(s.create("a.BTest"), terminal)

	st := run.Status
	if st.Reason != model.CompileFailed || st.Result.Compiled || len(st.Result.Output) > 600 || !strings.Contains(st.Result.Output, "BTest.java:[5,57] ';' expected") {
		t.Errorf("status = %+v", st)
	}
}

func TestConcurrentRunsAreLimitedAndExtraRunsWaitAsPending(t *testing.T) {
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	s := start(t, configFor(t, t.TempDir(), gw.URL, "limits: {maxConcurrent: 1}"), rt)

	a := s.create("a.BTest")
	b := s.create("a.BTest")
	s.waitFor(a, phaseIs(model.Running))
	time.Sleep(150 * time.Millisecond)

	if _, run := s.get(b); run.Status.Phase != model.Pending {
		t.Fatalf("second run is %s while the first runs", run.Status.Phase)
	}
	rt.OnStart = simulate(rt, "surefire-pass.xml", 0, "")
	data, _ := os.ReadFile(filepath.Join("..", "report", "testdata", "surefire-pass.xml"))
	_, first := s.get(a)
	ws := filepath.Join(filepath.Dir(rt.Created[0].Mounts[0].Source), first.ID, "src", "target", "surefire-reports")
	_ = os.MkdirAll(ws, 0o755)
	_ = os.WriteFile(filepath.Join(ws, "TEST-a.BTest.xml"), data, 0o644)
	rt.Finish(rt.ForRun(a)[0].ID, 0, "")

	s.waitFor(a, terminal)
	if run := s.waitFor(b, terminal); run.Status.Phase != model.Succeeded {
		t.Errorf("second run = %+v", run.Status)
	}
}

func TestRunsEndAsSandboxUnavailableWithoutRunsc(t *testing.T) {
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	rt.NoRunsc = true
	s := start(t, configFor(t, t.TempDir(), gw.URL, ""), rt)

	status, body := s.request("GET", "/healthz", "", false)
	run := s.waitFor(s.create("a.BTest"), terminal)

	if status != 200 || strings.TrimSpace(string(body)) != `{"runsc":false}` {
		t.Errorf("healthz = %d %s", status, body)
	}
	if run.Status.Phase != model.Error || run.Status.Reason != model.SandboxUnavailable || len(gw.requests) != 0 {
		t.Errorf("status = %+v, gateway calls %v", run.Status, gw.requests)
	}
}

func TestHealthzReportsRunsc(t *testing.T) {
	s := start(t, configFor(t, t.TempDir(), "http://127.0.0.1:1", ""), runtimetest.New())

	status, body := s.request("GET", "/healthz", "", false)

	if status != 200 || strings.TrimSpace(string(body)) != `{"runsc":true}` {
		t.Errorf("healthz = %d %s", status, body)
	}
	if status, _ := s.request("POST", "/internal/v1/runs", "{}", false); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated create = %d", status)
	}
}

func TestSourceAndProjectErrors(t *testing.T) {
	t.Run("gateway error", func(t *testing.T) {
		gw := newGateway(t, mavenProject)
		gw.status = http.StatusBadGateway
		s := start(t, configFor(t, t.TempDir(), gw.URL, ""), runtimetest.New())

		run := s.waitFor(s.create("a.BTest"), terminal)

		if run.Status.Reason != model.SourceUnavailable || !strings.Contains(run.Status.Message, "provider unavailable") {
			t.Errorf("status = %+v", run.Status)
		}
	})
	t.Run("not a maven or gradle project", func(t *testing.T) {
		gw := newGateway(t, map[string]string{"package.json": "{}"})
		s := start(t, configFor(t, t.TempDir(), gw.URL, ""), runtimetest.New())

		run := s.waitFor(s.create("a.BTest"), terminal)

		if run.Status.Reason != model.UnsupportedProject || !strings.Contains(run.Status.Message, "not supported") {
			t.Errorf("status = %+v", run.Status)
		}
	})
}

func TestTimeoutStopsTheContainer(t *testing.T) {
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	s := start(t, configFor(t, t.TempDir(), gw.URL, "limits: {timeout: 200ms}"), rt)

	id := s.create("a.BTest")
	run := s.waitFor(id, terminal)

	if run.Status.Phase != model.Failed || run.Status.Reason != model.Timeout {
		t.Errorf("status = %+v", run.Status)
	}
	waitUntil(t, func() bool { return rt.Count() == 0 })
	if len(rt.Stopped) != 1 {
		t.Errorf("stopped = %v", rt.Stopped)
	}
}

func TestDeleteRemovesContainerWorkspaceAndRecord(t *testing.T) {
	dir := t.TempDir()
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	s := start(t, configFor(t, dir, gw.URL, ""), rt)
	id := s.create("a.BTest")
	s.waitFor(id, phaseIs(model.Running))

	status, _ := s.request("DELETE", "/internal/v1/runs/"+id, "", true)

	if status != http.StatusAccepted {
		t.Fatalf("delete = %d", status)
	}
	waitUntil(t, func() bool { code, _ := s.get(id); return code == http.StatusNotFound })
	if rt.Count() != 0 {
		t.Error("container left")
	}
	if _, err := os.Stat(filepath.Join(dir, "ws", id)); err == nil {
		t.Error("workspace left")
	}
}

func TestRestartAdoptsRunningContainersAndCleansOrphans(t *testing.T) {
	dir := t.TempDir()
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	first := start(t, configFor(t, dir, gw.URL, ""), rt)
	id := first.create("a.BTest")
	first.waitFor(id, phaseIs(model.Running))
	first.stop()

	// While the controller was down the build finished, and a stray container appeared.
	data, _ := os.ReadFile(filepath.Join("..", "report", "testdata", "surefire-pass.xml"))
	reports := filepath.Join(dir, "ws", id, "src", "target", "surefire-reports")
	_ = os.MkdirAll(reports, 0o755)
	_ = os.WriteFile(filepath.Join(reports, "TEST-a.BTest.xml"), data, 0o644)
	rt.Finish(rt.ForRun(id)[0].ID, 0, "[INFO] BUILD SUCCESS")
	rt.Add("run_ffffffffffffffffffffffff", runtime.StateRunning)
	_ = os.MkdirAll(filepath.Join(dir, "ws", "run_eeeeeeeeeeeeeeeeeeeeeeee"), 0o755)

	second := start(t, configFor(t, dir, gw.URL, ""), rt)
	run := second.waitFor(id, terminal)

	if run.Status.Phase != model.Succeeded {
		t.Errorf("status = %+v", run.Status)
	}
	waitUntil(t, func() bool { return rt.Count() == 0 })
	waitUntil(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "ws", "run_eeeeeeeeeeeeeeeeeeeeeeee"))
		return err != nil
	})
}

func TestAllowListProxyIsServed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "pom") }))
	defer origin.Close()
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	}
	s := start(t, configFor(t, t.TempDir(), "http://127.0.0.1:1", ""), runtimetest.New(), app.WithProxyDialer(dial))

	connect := func(target string) int {
		conn, err := net.Dial("tcp", s.app.ProxyAddr())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	if code := connect("repo.maven.apache.org:443"); code != 200 {
		t.Errorf("allowed host = %d", code)
	}
	if code := connect("example.org:443"); code != http.StatusForbidden {
		t.Errorf("denied host = %d", code)
	}
}

func TestNewFailsWhenPortsAreTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()

	api := configFor(t, dir, "http://x", fmt.Sprintf("server: {listenAddr: %q}", ln.Addr().String()))
	if _, err := app.New(context.Background(), api, log, app.WithRuntime(runtimetest.New())); err == nil {
		t.Error("API port conflict accepted")
	}
	proxyCfg := configFor(t, dir, "http://x", fmt.Sprintf("network: {proxy: {listen: %q}}", ln.Addr().String()))
	if _, err := app.New(context.Background(), proxyCfg, log, app.WithRuntime(runtimetest.New())); err == nil {
		t.Error("proxy port conflict accepted")
	}
	bad := configFor(t, dir, "http://x", "")
	bad.Runtime.Containerd.Shim = "gvisor"
	if _, err := app.New(context.Background(), bad, log); err == nil {
		t.Error("bad containerd shim accepted")
	}
	bad = configFor(t, dir, "http://x", "")
	bad.DataDir = "/proc/nope/data"
	if _, err := app.New(context.Background(), bad, log, app.WithRuntime(runtimetest.New())); err == nil {
		t.Error("unusable data dir accepted")
	}
}

func TestOptionsAreApplied(t *testing.T) {
	gw := newGateway(t, mavenProject)
	rt := runtimetest.New()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	s := start(t, configFor(t, t.TempDir(), gw.URL, ""), rt, app.WithOCIRuntime("runc"), app.WithClock(func() time.Time { return now }))

	run := s.waitFor(s.create("a.BTest"), phaseIs(model.Running))

	if rt.Created[0].Runtime != "runc" || !run.Status.StartedAt.Equal(now) {
		t.Errorf("runtime %q startedAt %v", rt.Created[0].Runtime, run.Status.StartedAt)
	}
	if s.app.Handler() == nil {
		t.Error("no handler")
	}
}
