package config_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/config"
)

const requiredYAML = "security: {internalToken: i}\ngitGateway: {internalToken: g}\n"

func mustLoad(t *testing.T, overlays ...string) config.Config {
	t.Helper()
	docs := make([][]byte, len(overlays))
	for i, o := range overlays {
		docs[i] = []byte(o)
	}
	cfg, err := config.LoadYAML(docs...)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	return cfg
}

func TestLoadDefaults(t *testing.T) {
	cfg := mustLoad(t, requiredYAML)

	if cfg.ListenAddr != ":8080" || cfg.LogLevel != slog.LevelInfo || cfg.ShutdownTimeout != 10*time.Second || cfg.InternalToken != "i" {
		t.Errorf("server defaults = %+v", cfg)
	}
	wantLimits := config.Limits{Timeout: 5 * time.Minute, CPUs: 2, MemoryBytes: 2 << 30, Pids: 512, MaxConcurrent: 2, User: "1000:1000"}
	if cfg.Limits != wantLimits {
		t.Errorf("limits = %+v", cfg.Limits)
	}
	wantHosts := []string{"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "services.gradle.org", "downloads.gradle.org"}
	if !reflect.DeepEqual(cfg.Network.AllowedHosts, wantHosts) || cfg.Network.Name != "pdd-sandbox" ||
		cfg.Network.ProxyListen != ":3128" || cfg.Network.ProxyHost != "" {
		t.Errorf("network = %+v", cfg.Network)
	}
	if cfg.GitGateway.URL != "http://pdd-git-gateway:8080" || cfg.DataDir != "/data" || cfg.CacheDir != "/var/lib/pdd-sandbox/cache" || cfg.Workspace.Dir != "/var/lib/pdd-sandbox/workspaces" ||
		cfg.OutputMaxBytes != 65536 || cfg.WorkspaceRetention != time.Hour || cfg.ResyncInterval != 5*time.Second {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Images.Maven == "" || cfg.Images.Gradle == "" {
		t.Errorf("images = %+v", cfg.Images)
	}
	wantRuntime := config.Runtime{
		Containerd: config.ContainerdRuntime{
			Address: "/run/containerd/containerd.sock", Namespace: "pdd-sandbox", Shim: "runc", RunscPath: "/usr/bin/runsc",
			LogDir: "/var/lib/pdd-sandbox/logs", NetnsDir: "/run/pdd-sandbox/netns",
			CNIBinDir: "/opt/cni/bin", CNIDataDir: "/run/pdd-sandbox/cni", CNISubnet: "10.213.0.0/16",
		},
	}
	if cfg.Runtime != wantRuntime {
		t.Errorf("runtime = %+v", cfg.Runtime)
	}
}

func TestOverlaysMergeMapsAndReplaceLists(t *testing.T) {
	cfg := mustLoad(t, requiredYAML,
		"limits: {memory: 512Mi, cpus: 0.5}\nnetwork: {allowedHosts: [Mirror.Example.com, '*.corp.example']}\n",
		"limits: {maxConcurrent: 4}\n")

	if cfg.Limits.MemoryBytes != 512<<20 || cfg.Limits.CPUs != 0.5 || cfg.Limits.MaxConcurrent != 4 || cfg.Limits.Pids != 512 {
		t.Errorf("limits = %+v", cfg.Limits)
	}
	if !reflect.DeepEqual(cfg.Network.AllowedHosts, []string{"mirror.example.com", "*.corp.example"}) {
		t.Errorf("hosts = %v", cfg.Network.AllowedHosts)
	}
}

func TestMissingTokenIsReported(t *testing.T) {
	_, err := config.LoadYAML()
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"security.internalToken: required", "gitGateway.internalToken: required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}

func TestAllProblemsAreReportedTogether(t *testing.T) {
	_, err := config.LoadYAML([]byte(`
security: {internalToken: "  "}
server: {logLevel: loud, shutdownTimeout: 0s}
gitGateway: {url: "ftp://x"}
limits: {timeout: 0s, cpus: 0, memory: lots, pids: 0, maxConcurrent: 0, user: "root"}
workspace: {maxBytes: 0}
network: {name: "-bad", allowedHosts: ["https://x.com", "ok.example"], proxy: {listen: ""}}
runtime: {containerd: {namespace: "-x", shim: gvisor, runscPath: runsc, cni: {subnet: "10.0.0.0/30"}}}
`))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"security.internalToken: required", "gitGateway.internalToken: required", "server.logLevel", "server.shutdownTimeout",
		"gitGateway.url", "limits.timeout", "limits.cpus", "limits.memory", "limits.pids", "limits.maxConcurrent",
		"limits.user", "workspace.maxBytes", "network.name", "network.allowedHosts[0]",
		"network.proxy.listen", "runtime.containerd.namespace",
		"runtime.containerd.shim", "runtime.containerd.runscPath", "runtime.containerd.cni.subnet",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}

func TestDecodeProblemsAreReportedTogether(t *testing.T) {
	_, err := config.LoadYAML([]byte("server: {shutdownTimeout: soon}\nnope: 1\nlimits2: {a: 1}\nnetwork: {allowedHosts: [5]}\n"))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"server.shutdownTimeout: invalid duration", "nope: unknown key", "limits2: unknown key", "network.allowedHosts[0]: must be a string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%v", want, err)
		}
	}
}

func TestValueValidation(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"root numeric", "limits: {user: '0:0'}", "limits.user: must not be root"},
		{"user text", "limits: {user: 'bob'}", "limits.user"},
		{"small memory", "limits: {memory: 1Mi}", "limits.memory: must be at least"},
		{"bytes memory", "limits: {memory: '1073741824'}", ""},
		{"empty hosts", "network: {allowedHosts: []}", "network.allowedHosts: must list at least one host"},
		{"hosts not list", "network: {allowedHosts: x}", "network.allowedHosts: must be a list"},
		{"removed kind", "runtime: {kind: containerd}", "runtime.kind: unknown key"},
		{"removed docker", "runtime: {docker: {host: /var/run/docker.sock}}", "runtime.docker: unknown key"},
		{"runsc shim", "runtime: {containerd: {shim: runsc}}", ""},
		{"relative socket", "runtime: {containerd: {address: containerd.sock}}", "runtime.containerd.address: must be an absolute path"},
		{"ipv6 subnet", "runtime: {containerd: {cni: {subnet: 'fd00::/64'}}}", "runtime.containerd.cni.subnet"},
		{"subnet without prefix", "runtime: {containerd: {cni: {subnet: '10.213.0.0'}}}", "runtime.containerd.cni.subnet"},
		{"relative log dir", "runtime: {containerd: {logDir: logs}}", "runtime.containerd.logDir: must be an absolute path"},
		{"int for string", "workspace: {dir: 5}", "workspace.dir: must be a string"},
		{"mapping for scalar", "limits: {cpus: {a: 1}}", "limits.cpus: must be a single value"},
		{"scalar for mapping", "limits: 5", "limits: must be a mapping"},
		{"bad bool-like int", "limits: {pids: x}", "limits.pids: must be an integer"},
		{"duration type", "limits: {timeout: 5}", "limits.timeout: must be a duration string"},
		{"float type", "limits: {cpus: x}", "limits.cpus: must be a number"},
		{"huge size", "limits: {memory: 99999999999999999999Gi}", "limits.memory: size out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.LoadYAML([]byte(requiredYAML), []byte(tc.yaml))
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInvalidYAMLNamesTheDocument(t *testing.T) {
	_, err := config.LoadYAML([]byte("a: [1"))
	if err == nil || !strings.Contains(err.Error(), "overlay 1") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadReadsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.yaml")
	if err := os.WriteFile(path, []byte(requiredYAML+"server: {logLevel: debug}\nlimits: ~\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load([]string{path})
	if err != nil || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("cfg = %+v, err = %v", cfg.LogLevel, err)
	}
	if _, err := config.Load([]string{filepath.Join(dir, "missing.yaml")}); err == nil {
		t.Error("missing file accepted")
	}
}
