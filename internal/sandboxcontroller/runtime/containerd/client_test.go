package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/runtime"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Address: filepath.Join(dir, "none.sock"), Namespace: "pdd-sandbox", Shim: ShimRunc, RunscPath: filepath.Join(dir, "runsc"),
		LogDir: filepath.Join(dir, "logs"), NetnsDir: filepath.Join(dir, "netns"), CNIBinDir: filepath.Join(dir, "cni-bin"),
		CNIDataDir: filepath.Join(dir, "cni"), CNISubnet: "10.213.0.0/16", ProxyPort: 3128, ipForwardPath: filepath.Join(dir, "ip_forward"),
	}
}

func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestConfigValidation(t *testing.T) {
	good := testConfig(t)
	if _, err := New(good); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no address", func(c *Config) { c.Address = "" }, "Address must not be empty"},
		{"no namespace", func(c *Config) { c.Namespace = "" }, "Namespace must not be empty"},
		{"shim", func(c *Config) { c.Shim = "kata" }, `shim "kata"`},
		{"subnet", func(c *Config) { c.CNISubnet = "fd00::/64" }, "not an IPv4 CIDR"},
		{"small subnet", func(c *Config) { c.CNISubnet = "10.0.0.0/30" }, "smaller than /28"},
		{"no proxy port", func(c *Config) { c.ProxyPort = 0 }, "ProxyPort 0 is not a port"},
		{"no subnet", func(c *Config) { c.CNISubnet = "" }, "CNISubnet must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestOCIRuntimeMapping(t *testing.T) {
	cfg := testConfig(t)
	c := newTestClient(t, cfg)

	rt, err := c.ociRuntime("runsc")
	if err != nil || rt.Type != "io.containerd.runc.v2" || rt.Binary != cfg.RunscPath || rt.Options == nil {
		t.Errorf("runsc with the runc shim: %+v, %v", rt, err)
	}
	if rt, err = c.ociRuntime("runc"); err != nil || rt.Type != "io.containerd.runc.v2" || rt.Binary != "runc" || rt.Options != nil {
		t.Errorf("runc: %+v, %v", rt, err)
	}
	if _, err = c.ociRuntime("kata"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("unknown runtime: %v", err)
	}

	cfg.Shim = ShimRunsc
	c = newTestClient(t, cfg)
	if rt, err = c.ociRuntime("runsc"); err != nil || rt.Type != "io.containerd.runsc.v1" || rt.Binary != "containerd-shim-runsc-v1" || rt.Options != nil {
		t.Errorf("runsc shim: %+v, %v", rt, err)
	}
}

func TestUnreachableContainerdIsUnavailable(t *testing.T) {
	c := newTestClient(t, testConfig(t))
	ctx := context.Background()

	start := time.Now()
	if err := c.CheckRuntime(ctx, "runsc"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("CheckRuntime = %v", err)
	}
	if _, err := c.List(ctx); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("List = %v", err)
	}
	if _, err := c.Inspect(ctx, "pdd-sandbox-x"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("Inspect = %v", err)
	}
	if err := c.Stop(ctx, "pdd-sandbox-x", time.Second); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("Stop = %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("calls took %s", took)
	}
	if _, err := c.Inspect(ctx, "../x"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("Inspect of an invalid id = %v", err)
	}
}

func TestCheckRuntimeNeedsTheBinary(t *testing.T) {
	// The daemon check comes first, so a missing runsc is only reported for a reachable containerd;
	// here both are missing and the error names the daemon.
	c := newTestClient(t, testConfig(t))
	err := c.CheckRuntime(context.Background(), "runsc")
	if !errors.Is(err, runtime.ErrUnavailable) || !strings.Contains(err.Error(), "containerd at") {
		t.Errorf("error = %v", err)
	}
}

func TestEnsureNetwork(t *testing.T) {
	cfg := testConfig(t)
	c := newTestClient(t, cfg)
	fw := newFakeIptables(c)
	ctx := context.Background()

	if _, err := c.EnsureNetwork(ctx, "pdd-sandbox"); !errors.Is(err, runtime.ErrUnavailable) || !strings.Contains(err.Error(), "ptp") {
		t.Errorf("without plugins: %v", err)
	}

	if err := os.MkdirAll(cfg.CNIBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range cniPlugins {
		if err := os.WriteFile(filepath.Join(cfg.CNIBinDir, plugin), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cfg.ipForwardPath, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := c.EnsureNetwork(ctx, "pdd-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if info.Gateway != "10.213.0.1" {
		t.Errorf("info = %+v", info)
	}
	if data, _ := os.ReadFile(cfg.ipForwardPath); strings.TrimSpace(string(data)) != "0" {
		t.Errorf("ip_forward = %q", data)
	}
	if again, err := c.EnsureNetwork(ctx, "pdd-sandbox"); err != nil || again != info {
		t.Errorf("second call = %+v, %v", again, err)
	}
	if got := fw.chains["iptables"][chainInput]; !reflect.DeepEqual(got, []string{
		"-m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT", "-p tcp --dport 3128 -j ACCEPT", "-j DROP"}) {
		t.Errorf("input chain = %q", got)
	}
	if got := fw.chains["iptables"][chainForward]; !reflect.DeepEqual(got, []string{"-s 10.213.0.0/16 -j DROP", "-d 10.213.0.0/16 -j DROP"}) {
		t.Errorf("forward chain = %q", got)
	}
	if got := fw.parents["iptables"]["INPUT"]; !reflect.DeepEqual(got, []string{"-s 10.213.0.0/16 -j " + chainInput}) {
		t.Errorf("INPUT = %q", got)
	}
}

func TestFirewallIsFailClosed(t *testing.T) {
	cfg := testConfig(t)
	c := newTestClient(t, cfg)
	fw := newFakeIptables(c)
	ctx := context.Background()

	if err := c.ensureFirewall(ctx); err != nil {
		t.Fatal(err)
	}
	calls := len(fw.calls)
	if err := c.ensureFirewall(ctx); err != nil {
		t.Fatal(err)
	}
	for _, call := range fw.calls[calls:] {
		if strings.Contains(call, " -A ") || strings.Contains(call, " -I ") || strings.Contains(call, " -F ") || strings.Contains(call, " -N ") {
			t.Errorf("second call changed the rules: %s", call)
		}
	}

	// A port from an earlier configuration is replaced, and the jump is not added twice.
	cfg.ProxyPort = 3129
	c2 := newTestClient(t, cfg)
	c2.run, c2.lookPath = c.run, c.lookPath
	if err := c2.ensureFirewall(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fw.chains["iptables"][chainInput]; !strings.Contains(strings.Join(got, ";"), "--dport 3129") || strings.Contains(strings.Join(got, ";"), "3128") {
		t.Errorf("input chain after port change = %q", got)
	}
	if got := fw.parents["iptables"]["INPUT"]; len(got) != 1 {
		t.Errorf("INPUT jumps = %q", got)
	}

	fw.fail = "-A"
	fw.chains = map[string]map[string][]string{}
	fw.parents = map[string]map[string][]string{}
	if err := c.ensureFirewall(ctx); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("rule that cannot be installed: %v", err)
	}
	c.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if err := c.ensureFirewall(ctx); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("missing iptables: %v", err)
	}
}

// fakeIptables keeps the chains that iptables and ip6tables commands build.
type fakeIptables struct {
	chains  map[string]map[string][]string
	parents map[string]map[string][]string
	calls   []string
	fail    string
}

func newFakeIptables(c *Client) *fakeIptables {
	f := &fakeIptables{chains: map[string]map[string][]string{}, parents: map[string]map[string][]string{}}
	c.lookPath = func(name string) (string, error) { return name, nil }
	c.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		return f.exec(filepath.Base(name), args[2:])
	}
	return f
}

func (f *fakeIptables) exec(bin string, args []string) ([]byte, error) {
	if f.chains[bin] == nil {
		f.chains[bin], f.parents[bin] = map[string][]string{}, map[string][]string{"INPUT": nil, "FORWARD": nil}
	}
	chains, parents := f.chains[bin], f.parents[bin]
	op, name, rest := args[0], args[1], strings.Join(args[2:], " ")
	if f.fail == op {
		return []byte("boom"), errors.New("exit status 4")
	}
	_, isChain := chains[name]
	_, isParent := parents[name]
	switch op {
	case "-S":
		if !isChain {
			return nil, errors.New("no such chain")
		}
		out := "-N " + name + "\n"
		for _, r := range chains[name] {
			out += "-A " + name + " " + r + "\n"
		}
		return []byte(out), nil
	case "-N":
		chains[name] = []string{}
	case "-F":
		chains[name] = []string{}
	case "-A":
		chains[name] = append(chains[name], rest)
	case "-C":
		list := chains[name]
		if isParent {
			list = parents[name]
		}
		if !slices.Contains(list, rest) {
			return nil, errors.New("no such rule")
		}
	case "-I":
		parents[name] = append([]string{strings.TrimPrefix(rest, "1 ")}, parents[name]...)
	}
	return nil, nil
}

func TestPluginMustBeExecutable(t *testing.T) {
	cfg := testConfig(t)
	c := newTestClient(t, cfg)
	if err := os.MkdirAll(cfg.CNIBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, plugin := range cniPlugins {
		if err := os.WriteFile(filepath.Join(cfg.CNIBinDir, plugin), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.EnsureNetwork(context.Background(), "pdd-sandbox"); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("non-executable plugins: %v", err)
	}
}

func TestForwardingThatCannotBeSwitchedOffIsUnavailable(t *testing.T) {
	cfg := testConfig(t)
	cfg.ipForwardPath = filepath.Join(t.TempDir(), "missing", "ip_forward")
	c := newTestClient(t, cfg)
	if err := c.disableForwarding(); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("missing sysctl file: %v", err)
	}

	dir := t.TempDir()
	cfg.ipForwardPath = dir
	c = newTestClient(t, cfg)
	if err := c.disableForwarding(); !errors.Is(err, runtime.ErrUnavailable) {
		t.Errorf("unreadable sysctl file: %v", err)
	}

	file := filepath.Join(t.TempDir(), "ip_forward")
	if err := os.WriteFile(file, []byte("1\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		cfg.ipForwardPath = file
		c = newTestClient(t, cfg)
		if err := c.disableForwarding(); !errors.Is(err, runtime.ErrUnavailable) {
			t.Errorf("read-only sysctl file: %v", err)
		}
	}
}

func TestCNIConfig(t *testing.T) {
	data, gateway, err := cniConfig("pdd-sandbox", "10.213.7.9/16", "/run/pdd-sandbox/cni")
	if err != nil {
		t.Fatal(err)
	}
	if gateway != netip.MustParseAddr("10.213.0.1") {
		t.Errorf("gateway = %s", gateway)
	}
	var conf struct {
		CNIVersion string `json:"cniVersion"`
		Name       string `json:"name"`
		Plugins    []struct {
			Type   string `json:"type"`
			IPMasq bool   `json:"ipMasq"`
			IPAM   struct {
				Type    string `json:"type"`
				DataDir string `json:"dataDir"`
				Ranges  [][]struct {
					Subnet  string `json:"subnet"`
					Gateway string `json:"gateway"`
				} `json:"ranges"`
			} `json:"ipam"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &conf); err != nil {
		t.Fatal(err)
	}
	if conf.Name != "pdd-sandbox" || conf.CNIVersion != "1.0.0" || len(conf.Plugins) != 1 {
		t.Fatalf("conf = %s", data)
	}
	p := conf.Plugins[0]
	if p.Type != "ptp" || p.IPMasq || p.IPAM.Type != "host-local" || p.IPAM.DataDir != "/run/pdd-sandbox/cni" {
		t.Errorf("plugin = %+v", p)
	}
	if r := p.IPAM.Ranges; len(r) != 1 || len(r[0]) != 1 || r[0][0].Subnet != "10.213.0.0/16" || r[0][0].Gateway != "10.213.0.1" {
		t.Errorf("ranges = %+v", r)
	}
}

func TestCNIConfigRejectsBadSubnets(t *testing.T) {
	for _, subnet := range []string{"", "10.213.0.0", "fd00::/64", "10.213.0.0/29", "10.213.0.0/33", "banana"} {
		if _, _, err := cniConfig("n", subnet, "/d"); err == nil {
			t.Errorf("%q accepted", subnet)
		}
	}
	if _, g, err := cniConfig("n", "192.168.4.0/28", "/d"); err != nil || g.String() != "192.168.4.1" {
		t.Errorf("/28: %v, %v", g, err)
	}
}

func TestHostsFile(t *testing.T) {
	got := string(hostsFile("sandbox-1", netip.MustParseAddr("10.213.0.2")))
	want := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n10.213.0.2\tsandbox-1\n"
	if got != want {
		t.Errorf("hosts = %q", got)
	}
	if got := string(hostsFile("sandbox-1", netip.Addr{})); strings.Contains(got, "sandbox-1") {
		t.Errorf("hosts without address = %q", got)
	}
}

func TestOOMKillCount(t *testing.T) {
	cases := map[string]int{
		"low 0\nhigh 0\nmax 12\noom 3\noom_kill 2\noom_group_kill 0\n": 2,
		"oom_kill_disable 0\nunder_oom 0\noom_kill 1\n":                1,
		"oom_kill_disable 0\nunder_oom 0\n":                            0,
		"oom_kill x\n":                                                 0,
		"":                                                             0,
	}
	for in, want := range cases {
		if got := oomKillCount(in); got != want {
			t.Errorf("oomKillCount(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTailFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		n    int
		want string
	}{{4, "6789"}, {10, "0123456789"}, {100, "0123456789"}, {0, ""}} {
		if got, err := tailFile(path, tc.n); err != nil || got != tc.want {
			t.Errorf("tailFile(%d) = %q, %v; want %q", tc.n, got, err, tc.want)
		}
	}
	if _, err := tailFile(filepath.Join(t.TempDir(), "none"), 4); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestLogsOfUnknownContainer(t *testing.T) {
	cfg := testConfig(t)
	c := newTestClient(t, cfg)
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.LogDir, "pdd-sandbox-a.log"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Logs(context.Background(), "pdd-sandbox-a", 5); err != nil || got != "world" {
		t.Errorf("Logs = %q, %v", got, err)
	}
}

func TestKeyedMutexSerializesPerKey(t *testing.T) {
	var k keyedMutex
	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := k.lock("same")
			if inside.Add(1) > 1 {
				overlaps.Add(1)
			}
			time.Sleep(2 * time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	unlockOther := k.lock("other")
	unlockOther()
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Errorf("%d overlapping holders", overlaps.Load())
	}
	if len(k.locks) != 0 {
		t.Errorf("%d locks leaked", len(k.locks))
	}
}

func TestCreateRejectsBadNames(t *testing.T) {
	c := newTestClient(t, testConfig(t))
	for _, name := range []string{"", "../x", "a/b", "-x", strings.Repeat("a", 200)} {
		if _, err := c.Create(context.Background(), runtime.ContainerSpec{Name: name, Image: "x", Runtime: "runsc"}); err == nil || errors.Is(err, runtime.ErrUnavailable) {
			t.Errorf("name %q: %v", name, err)
		}
	}
}
