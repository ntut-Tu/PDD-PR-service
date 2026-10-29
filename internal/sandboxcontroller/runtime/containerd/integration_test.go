//go:build containerdit

package containerd_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/runtime"
	"pdd/services/internal/sandboxcontroller/runtime/containerd"
)

// These tests need root, a running containerd and the CNI plugins: run them in the runtime
// container (see cmd/sandbox-controller/README.md). They are excluded from normal runs: build
// with -tags containerdit.
//
//	PDD_IT_CONTAINERD  containerd socket, default /run/containerd/containerd.sock
//	PDD_IT_NAMESPACE   containerd namespace, default pdd-sandbox-it
//	PDD_IT_RUNTIME     OCI runtime, default runsc (runc runs without gVisor and skips its checks)
//	PDD_IT_RUNSC       runsc binary, default /usr/bin/runsc
//	PDD_IT_IMAGE       image with bash, coreutils and dmesg, default docker.io/library/ubuntu:24.04;
//	                   it must be in the namespace already (ctr -n <ns> images import)
//	PDD_IT_CNI_BIN     CNI plugin directory, default /opt/cni/bin
//	PDD_IT_SUBNET      sandbox subnet, default 10.214.0.0/24
const network = "pdd-sandbox-it"

type env struct {
	proxy   net.Listener
	t       *testing.T
	cfg     containerd.Config
	client  *containerd.Client
	oci     string
	image   string
	gateway string
	net     runtime.NetworkInfo
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newEnv(t *testing.T) *env {
	t.Helper()
	address := getenv("PDD_IT_CONTAINERD", "/run/containerd/containerd.sock")
	if _, err := os.Stat(address); err != nil {
		t.Skipf("no containerd socket: %v", err)
	}
	base, err := os.MkdirTemp("/run", "pdd-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	proxy, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	go serveBanner(proxy)
	cfg := containerd.Config{
		ProxyPort: proxy.Addr().(*net.TCPAddr).Port,
		Address:   address, Namespace: getenv("PDD_IT_NAMESPACE", "pdd-sandbox-it"), Shim: containerd.ShimRunc,
		RunscPath: getenv("PDD_IT_RUNSC", "/usr/bin/runsc"), LogDir: filepath.Join(base, "logs"),
		NetnsDir: filepath.Join(base, "netns"), CNIBinDir: getenv("PDD_IT_CNI_BIN", "/opt/cni/bin"),
		CNIDataDir: filepath.Join(base, "cni"), CNISubnet: getenv("PDD_IT_SUBNET", "10.214.0.0/24"),
	}
	client, err := containerd.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	e := &env{proxy: proxy, t: t, cfg: cfg, client: client, oci: getenv("PDD_IT_RUNTIME", "runsc"), image: getenv("PDD_IT_IMAGE", "docker.io/library/ubuntu:24.04")}
	if err := client.CheckRuntime(context.Background(), e.oci); err != nil {
		t.Skipf("runtime %s not available: %v", e.oci, err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		t.Fatalf("enable forwarding to prove that the runtime turns it off: %v", err)
	}
	if e.net, err = client.EnsureNetwork(context.Background(), network); err != nil {
		t.Fatal(err)
	}
	e.gateway = e.net.Gateway
	t.Cleanup(func() {
		ctx := context.Background()
		list, _ := client.List(ctx)
		for _, ct := range list {
			_ = client.Remove(ctx, ct.ID)
		}
	})
	return e
}

// serveBanner answers every connection with a line, standing in for the proxy and the API.
func serveBanner(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("hello-from-" + ln.Addr().String() + "\n"))
		_ = conn.Close()
	}
}

func (e *env) spec(name, script string) runtime.ContainerSpec {
	return runtime.ContainerSpec{
		Name: name, Image: e.image, Labels: map[string]string{runtime.LabelRun: "run-" + name},
		Entrypoint: []string{"bash", "-c"}, Cmd: []string{script}, User: "1000:1000",
		Runtime: e.oci, Network: network, WorkingDir: "/tmp",
		Tmpfs: map[string]string{"/tmp": "rw,exec,nosuid,size=64m"},
		CPUs:  1.5, MemoryBytes: 256 << 20, PidsLimit: 128,
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, ReadonlyRootfs: true,
	}
}

func (e *env) create(spec runtime.ContainerSpec) string {
	e.t.Helper()
	id, err := e.client.Create(context.Background(), spec)
	if err != nil {
		e.t.Fatalf("Create %s: %v", spec.Name, err)
	}
	return id
}

func (e *env) start(id string) {
	e.t.Helper()
	if err := e.client.Start(context.Background(), id); err != nil {
		e.t.Fatalf("Start %s: %v", id, err)
	}
}

func (e *env) wait(id string) runtime.Container {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ct, err := e.client.Inspect(context.Background(), id)
		if err != nil {
			e.t.Fatalf("Inspect %s: %v", id, err)
		}
		if ct.State == runtime.StateExited {
			return ct
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s did not exit: %+v", id, ct)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *env) logs(id string) string {
	e.t.Helper()
	out, err := e.client.Logs(context.Background(), id, 1<<16)
	if err != nil {
		e.t.Fatalf("Logs %s: %v", id, err)
	}
	return out
}

// run creates, starts and waits for a container.
func (e *env) run(spec runtime.ContainerSpec) (runtime.Container, string) {
	e.t.Helper()
	id := e.create(spec)
	e.start(id)
	ct := e.wait(id)
	return ct, e.logs(id)
}

func (e *env) gVisor() bool { return e.oci == "runsc" }

func TestCheckRuntimeAndNetwork(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if e.net.Gateway != "10.214.0.1" && os.Getenv("PDD_IT_SUBNET") == "" {
		t.Errorf("gateway = %q", e.net.Gateway)
	}
	if data, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); strings.TrimSpace(string(data)) != "0" {
		t.Errorf("ip_forward = %q after EnsureNetwork", data)
	}
	if err := e.client.CheckRuntime(ctx, "nonsense"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("unknown runtime: %v", err)
	}

	missing := e.cfg
	missing.RunscPath = "/nonexistent/runsc"
	c, err := containerd.New(missing)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.CheckRuntime(ctx, "runsc"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("missing runsc: %v", err)
	}

	unreachable := e.cfg
	unreachable.Address = filepath.Join(t.TempDir(), "none.sock")
	u, err := containerd.New(unreachable)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = u.Close() }()
	if err := u.CheckRuntime(ctx, e.oci); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("unreachable containerd: %v", err)
	}
	if _, err := u.List(ctx); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("List on unreachable containerd: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	for _, d := range []string{"ro", "rw"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(dir, d), 0o777)
	}
	_ = os.WriteFile(filepath.Join(dir, "ro", "in.txt"), []byte("input"), 0o644)

	spec := e.spec("lifecycle", `
echo "out-line"; echo "err-line" >&2
echo "uid=$(id -u) gid=$(id -g) cwd=$PWD home=$HOME"
echo "ro=$(cat /ro/in.txt)"; echo written > /rw/out.txt && echo "rw=ok"
(echo x > /ro/no.txt) 2>/dev/null && echo "ro-writable" || echo "ro=readonly"
(echo x > /usr/probe) 2>/dev/null && echo "root-writable" || echo "root=readonly"
echo y > /tmp/probe && echo "tmp=ok"
echo "caps=$(grep CapEff /proc/self/status | cut -f2) nnp=$(grep NoNewPrivs /proc/self/status | cut -f2)"
echo "hosts=$(cat /etc/hosts | tr '\n' ' ')"
exit 3`)
	spec.Mounts = []runtime.Mount{{Source: filepath.Join(dir, "ro"), Target: "/ro", ReadOnly: true}, {Source: filepath.Join(dir, "rw"), Target: "/rw"}}
	spec.Env = []string{"HOME=/tmp", "PDD_PROBE=1"}

	id := e.create(spec)
	if id != "lifecycle" {
		t.Errorf("id = %q", id)
	}
	if again, err := e.client.Create(ctx, spec); err != nil || again != id {
		t.Errorf("second Create = %q, %v", again, err)
	}
	ct, err := e.client.Inspect(ctx, id)
	if err != nil || ct.State != runtime.StateCreated || ct.RunID != "run-lifecycle" || ct.Name != id {
		t.Fatalf("created: %+v, %v", ct, err)
	}
	if _, err := e.client.Logs(ctx, id, 100); err != nil {
		t.Errorf("Logs before start: %v", err)
	}
	e.start(id)
	if err := e.client.Start(ctx, id); err != nil {
		t.Errorf("Start of a running or finished container: %v", err)
	}
	ct = e.wait(id)
	out := e.logs(id)
	t.Logf("exit=%d oom=%v started=%s finished=%s\n%s", ct.ExitCode, ct.OOMKilled, ct.StartedAt, ct.FinishedAt, out)

	if ct.ExitCode != 3 || ct.OOMKilled || ct.StartedAt.IsZero() || !ct.FinishedAt.After(ct.StartedAt) || ct.RunID != "run-lifecycle" {
		t.Errorf("exited: %+v", ct)
	}
	for _, want := range []string{
		"out-line", "err-line", "uid=1000 gid=1000 cwd=/tmp home=/tmp", "ro=input", "rw=ok", "ro=readonly", "root=readonly", "tmp=ok",
		"caps=0000000000000000", "nnp=1", "127.0.0.1\tlocalhost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "rw", "out.txt")); string(got) != "written\n" {
		t.Errorf("bind mount content = %q", got)
	}
	tail, err := e.client.Logs(ctx, id, 10)
	if err != nil || tail != out[len(out)-10:] {
		t.Errorf("tail = %q, %v", tail, err)
	}

	list, err := e.client.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].State != runtime.StateExited {
		t.Errorf("List = %+v, %v", list, err)
	}

	if err := e.client.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.Inspect(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Inspect after Remove: %v", err)
	}
	if _, err := e.client.Logs(ctx, id, 10); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Logs after Remove: %v", err)
	}
	if err := e.client.Remove(ctx, id); err != nil {
		t.Errorf("second Remove: %v", err)
	}
	e.assertClean(t, id)
}

// assertClean checks that nothing of the containers is left behind.
func (e *env) assertClean(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		for _, ext := range []string{".log", ".hosts"} {
			if _, err := os.Stat(filepath.Join(e.cfg.LogDir, id+ext)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s%s left behind: %v", id, ext, err)
			}
		}
	}
	if entries, _ := os.ReadDir(e.cfg.NetnsDir); len(entries) != 0 {
		t.Errorf("network namespaces left behind: %v", entries)
	}
	leases, _ := filepath.Glob(filepath.Join(e.cfg.CNIDataDir, network, "*"))
	for _, lease := range leases {
		if ip := filepath.Base(lease); strings.Count(ip, ".") == 3 {
			t.Errorf("address lease left behind: %s", lease)
		}
	}
	if links, _ := filepath.Glob("/sys/class/net/veth*"); len(links) != 0 {
		t.Errorf("veth links left behind: %v", links)
	}
	if list, err := e.client.List(context.Background()); err != nil || len(list) != 0 {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestGVisorAndLimits(t *testing.T) {
	e := newEnv(t)
	spec := e.spec("limits", `echo "dmesg:"; dmesg 2>&1 | head -5; echo "version: $(cat /proc/version)"; sleep 30`)
	spec.User = "0:0"
	spec.CapDrop, spec.SecurityOpt = nil, nil
	id := e.create(spec)
	e.start(id)
	defer func() { _ = e.client.Remove(context.Background(), id) }()

	deadline := time.Now().Add(20 * time.Second)
	var out string
	for !strings.Contains(out, "version:") && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		out = e.logs(id)
	}
	t.Log(out)
	if e.gVisor() && !strings.Contains(strings.ToLower(out), "gvisor") {
		t.Errorf("guest dmesg does not show gVisor:\n%s", out)
	}
	if !e.gVisor() && strings.Contains(strings.ToLower(out), "gvisor") {
		t.Errorf("gVisor used with runtime %s", e.oci)
	}

	limits := cgroupLimits(t, "/pdd-sandbox-it/"+id)
	want := map[string]string{"memory": strconv.Itoa(256 << 20), "swap": strconv.Itoa(256 << 20), "cpu": "150000/100000", "pids": "128"}
	for k, v := range want {
		if limits[k] != v {
			t.Errorf("cgroup %s = %q, want %q (all: %v)", k, limits[k], v, limits)
		}
	}
}

// cgroupLimits reads the limits of a container's cgroup, on cgroup v2 or v1.
func cgroupLimits(t *testing.T, path string) map[string]string {
	t.Helper()
	read := func(parts ...string) string {
		data, err := os.ReadFile(filepath.Join(append([]string{"/sys/fs/cgroup"}, parts...)...))
		if err != nil {
			return "?"
		}
		return strings.TrimSpace(string(data))
	}
	out := map[string]string{}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		out["memory"], out["pids"] = read(path, "memory.max"), read(path, "pids.max")
		out["swap"] = "?"
		if swap := read(path, "memory.swap.max"); swap == "0" {
			out["swap"] = out["memory"]
		}
		quota, period, _ := strings.Cut(read(path, "cpu.max"), " ")
		out["cpu"] = quota + "/" + period
		return out
	}
	out["memory"] = read("memory", path, "memory.limit_in_bytes")
	out["swap"] = read("memory", path, "memory.memsw.limit_in_bytes")
	out["pids"] = read("pids", path, "pids.max")
	out["cpu"] = read("cpu", path, "cpu.cfs_quota_us") + "/" + read("cpu", path, "cpu.cfs_period_us")
	return out
}

func TestOOMKilled(t *testing.T) {
	e := newEnv(t)
	spec := e.spec("oom", `tail /dev/zero`)
	spec.MemoryBytes = 64 << 20
	ct, out := e.run(spec)
	t.Logf("%+v\n%s", ct, out)
	if ct.ExitCode == 0 || !ct.OOMKilled {
		t.Errorf("memory hog: %+v", ct)
	}
}

func TestStop(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	polite := e.spec("polite", `trap 'echo term; exit 0' TERM; echo ready; while true; do sleep 0.1; done`)
	stubborn := e.spec("stubborn", `trap '' TERM; echo ready; while true; do sleep 0.1; done`)
	for _, spec := range []runtime.ContainerSpec{polite, stubborn} {
		id := e.create(spec)
		e.start(id)
		for i := 0; !strings.Contains(e.logs(id), "ready") && i < 100; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	start := time.Now()
	if err := e.client.Stop(ctx, "polite", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("polite container took %s to stop", took)
	}
	ct, _ := e.client.Inspect(ctx, "polite")
	if ct.State != runtime.StateExited || !strings.Contains(e.logs("polite"), "term") {
		t.Errorf("polite: %+v", ct)
	}

	start = time.Now()
	if err := e.client.Stop(ctx, "stubborn", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 2*time.Second || took > 20*time.Second {
		t.Errorf("stubborn container took %s to stop", took)
	}
	ct, _ = e.client.Inspect(ctx, "stubborn")
	if ct.State != runtime.StateExited || ct.ExitCode != 137 {
		t.Errorf("stubborn: %+v", ct)
	}

	for _, id := range []string{"polite", "stubborn", "never-existed"} {
		if err := e.client.Stop(ctx, id, time.Second); err != nil {
			t.Errorf("Stop %s again: %v", id, err)
		}
	}
	for _, id := range []string{"polite", "stubborn"} {
		if err := e.client.Remove(ctx, id); err != nil {
			t.Errorf("Remove %s: %v", id, err)
		}
	}
	e.assertClean(t, "polite", "stubborn")
}

func TestRemoveRunningContainer(t *testing.T) {
	e := newEnv(t)
	id := e.create(e.spec("busy", `sleep 300`))
	e.start(id)
	if ct, _ := e.client.Inspect(context.Background(), id); ct.State != runtime.StateRunning {
		t.Fatalf("not running: %+v", ct)
	}
	if err := e.client.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	e.assertClean(t, id)
}

func TestNetworkIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Stands in for the API of the controller, on a port the policy does not open.
	api, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = api.Close() }()
	go serveBanner(api)
	port := e.proxy.Addr().(*net.TCPAddr).Port
	apiPort := api.Addr().(*net.TCPAddr).Port

	// A second sandbox gives the first one a neighbour to try to reach.
	other := e.create(e.spec("neighbour", `sleep 120`))
	e.start(other)
	hosts, err := os.ReadFile(filepath.Join(e.cfg.LogDir, other+".hosts"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(strings.Split(strings.TrimSpace(string(hosts)), "\n")[2])
	neighbour := fields[0]

	script := fmt.Sprintf(`
probe() { local s=$(date +%%s%%N); if timeout 4 bash -c "exec 3<>/dev/tcp/$1/$2 && head -n1 <&3" 2>/tmp/err; then r=connected; else r="failed($?): $(tr -d '\n' </tmp/err)"; fi; echo "$3: $r in $(( ($(date +%%s%%N)-s)/1000000 ))ms"; }
echo "ip: $(grep -c . /etc/hosts) $(tail -n1 /etc/hosts)"
probe %[1]s %[2]d proxy
probe 1.1.1.1 443 internet
probe 8.8.8.8 53 dns
probe %[3]s %[2]d neighbour
probe %[1]s %[4]d api
probe %[1]s 9 gateway-closed-port`, e.gateway, port, neighbour, apiPort)
	ct, out := e.run(e.spec("probe", script))
	t.Logf("neighbour=%s gateway=%s proxy=%d api=%d exit=%d\n%s", neighbour, e.gateway, port, apiPort, ct.ExitCode, out)

	if !strings.Contains(out, "proxy: connected") || !strings.Contains(out, "hello-from-") {
		t.Errorf("the sandbox cannot reach the host side of its link:\n%s", out)
	}
	for _, name := range []string{"internet", "dns"} {
		if !strings.Contains(out, name+": failed") {
			t.Errorf("%s reachable:\n%s", name, out)
		}
	}
	if !strings.Contains(out, "neighbour: failed(124)") {
		t.Errorf("neighbour is not silently dropped (a refused connection means the packet got there):\n%s", out)
	}
	if !strings.Contains(out, "api: failed(124)") {
		t.Errorf("the API port must be dropped, not answered or refused:\n%s", out)
	}
	if !strings.Contains(out, "gateway-closed-port: failed(124)") {
		t.Errorf("every port but the proxy's must be dropped:\n%s", out)
	}
	if data, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); strings.TrimSpace(string(data)) != "0" {
		t.Errorf("ip_forward = %q while sandboxes exist", data)
	}

	// The firewall holds even while forwarding is forced on: the neighbour stays unreachable.
	control := e.create(e.spec("control", fmt.Sprintf(`timeout 4 bash -c "exec 3<>/dev/tcp/%s/%d" 2>&1; echo "rc=$?"`, neighbour, port)))
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("0\n"), 0o644) }()
	e.start(control)
	e.wait(control)
	if out := e.logs(control); !strings.Contains(out, "rc=124") {
		t.Errorf("neighbour with ip_forward=1: %q", out)
	}
	if err := e.client.Remove(ctx, "control"); err != nil {
		t.Error(err)
	}
	if err := e.client.Remove(ctx, "probe"); err != nil {
		t.Error(err)
	}
	if err := e.client.Remove(ctx, other); err != nil {
		t.Error(err)
	}
	e.assertClean(t, "probe", "control", other)
}

func TestConcurrentContainers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const n = 4
	ids := make([]string, n)
	var wg sync.WaitGroup
	errs := make(chan error, 3*n)
	for i := range ids {
		ids[i] = fmt.Sprintf("conc-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := e.client.Create(ctx, e.spec(ids[i], `sleep 60`))
			if err == nil {
				err = e.client.Start(ctx, id)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	for _, id := range ids {
		hosts, _ := os.ReadFile(filepath.Join(e.cfg.LogDir, id+".hosts"))
		ip := strings.Fields(strings.Split(strings.TrimSpace(string(hosts)), "\n")[2])[0]
		if other, dup := seen[ip]; dup {
			t.Errorf("%s and %s share %s", id, other, ip)
		}
		seen[ip] = id
	}
	list, err := e.client.List(ctx)
	if err != nil || len(list) != n {
		t.Errorf("List = %d containers, %v", len(list), err)
	}
	for _, ct := range list {
		if ct.State != runtime.StateRunning {
			t.Errorf("%+v", ct)
		}
	}

	// A second client on the same containerd adopts them, as the controller does after a restart.
	adopter, err := containerd.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = adopter.Close() }()
	if adopted, err := adopter.List(ctx); err != nil || len(adopted) != n {
		t.Errorf("adopter List = %d containers, %v", len(adopted), err)
	}
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := adopter.Remove(ctx, id); err != nil {
				t.Errorf("Remove %s: %v", id, err)
			}
		}()
	}
	wg.Wait()
	e.assertClean(t, ids...)
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	bad := e.spec("../escape", "true")
	if _, err := e.client.Create(ctx, bad); err == nil {
		t.Error("path in container name accepted")
	}
	bad = e.spec("badopt", "true")
	bad.SecurityOpt = []string{"seccomp=unconfined"}
	if _, err := e.client.Create(ctx, bad); err == nil {
		t.Error("unsupported security option accepted")
	}
	bad = e.spec("unknown-runtime", "true")
	bad.Runtime = "kata"
	if _, err := e.client.Create(ctx, bad); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("unknown OCI runtime: %v", err)
	}
	bad = e.spec("no-image", "true")
	bad.Image = "not a valid reference!"
	if _, err := e.client.Create(ctx, bad); err == nil {
		t.Error("invalid image reference accepted")
	}
	if err := e.client.Start(ctx, "ghost"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Start unknown: %v", err)
	}
	if _, err := e.client.Inspect(ctx, "ghost"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Inspect unknown: %v", err)
	}
	e.assertClean(t, "../escape", "badopt", "unknown-runtime", "no-image")
}
