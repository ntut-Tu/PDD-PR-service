package containerd

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"pdd/services/internal/sandboxcontroller/runtime"
)

func buildSpec(t *testing.T, spec runtime.ContainerSpec, files sandboxFiles) *oci.Spec {
	t.Helper()
	opts, err := specOpts(spec, nil, files)
	if err != nil {
		t.Fatalf("specOpts: %v", err)
	}
	ctx := namespaces.WithNamespace(context.Background(), "pdd-sandbox")
	out, err := oci.GenerateSpecWithPlatform(ctx, nil, "linux/amd64", &containers.Container{ID: spec.Name}, opts...)
	if err != nil {
		t.Fatalf("GenerateSpec: %v", err)
	}
	return out
}

func sandboxSpec() runtime.ContainerSpec {
	return runtime.ContainerSpec{
		Name: "pdd-sandbox-run_0123", Image: "maven:3.9", Entrypoint: []string{"mvn"}, Cmd: []string{"-B", "test"},
		Env: []string{"HOME=/cache/home", "MAVEN_OPTS=-Xmx1g"}, WorkingDir: "/workspace/src", User: "1000:1000",
		Mounts: []runtime.Mount{
			{Source: "/var/lib/pdd-sandbox/workspaces/run_0123", Target: "/workspace"},
			{Source: "/var/lib/pdd-sandbox/cache/7/1", Target: "/cache", ReadOnly: false},
			{Source: "/etc/ssl", Target: "/ssl", ReadOnly: true},
		},
		Tmpfs: map[string]string{"/tmp": "rw,exec,nosuid,size=512m", "/run": " rw , noexec "},
		CPUs:  2, MemoryBytes: 2 << 30, PidsLimit: 512,
		CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, ReadonlyRootfs: true,
	}
}

func TestSpecOptsMapContainerSpec(t *testing.T) {
	files := sandboxFiles{Netns: "/run/pdd-sandbox/netns/cni-1", Hosts: "/var/lib/pdd-sandbox/logs/x.hosts"}
	s := buildSpec(t, sandboxSpec(), files)

	if !reflect.DeepEqual(s.Process.Args, []string{"mvn", "-B", "test"}) || s.Process.Cwd != "/workspace/src" {
		t.Errorf("process = %v in %s", s.Process.Args, s.Process.Cwd)
	}
	if s.Process.User.UID != 1000 || s.Process.User.GID != 1000 {
		t.Errorf("user = %+v", s.Process.User)
	}
	env := strings.Join(s.Process.Env, " ")
	if !strings.Contains(env, "HOME=/cache/home") || !strings.Contains(env, "MAVEN_OPTS=-Xmx1g") {
		t.Errorf("env = %v", s.Process.Env)
	}
	if s.Hostname != "pdd-sandbox-run_0123" || !s.Root.Readonly || !s.Process.NoNewPrivileges {
		t.Errorf("hostname=%q readonly=%v nnp=%v", s.Hostname, s.Root.Readonly, s.Process.NoNewPrivileges)
	}
	caps := s.Process.Capabilities
	for name, set := range map[string][]string{"bounding": caps.Bounding, "effective": caps.Effective, "permitted": caps.Permitted, "inheritable": caps.Inheritable, "ambient": caps.Ambient} {
		if len(set) != 0 {
			t.Errorf("%s capabilities = %v", name, set)
		}
	}

	r := s.Linux.Resources
	if *r.CPU.Quota != 200_000 || *r.CPU.Period != 100_000 {
		t.Errorf("cpu = %d/%d", *r.CPU.Quota, *r.CPU.Period)
	}
	if *r.Memory.Limit != 2<<30 || *r.Memory.Swap != 2<<30 || r.Pids.Limit == nil || *r.Pids.Limit != 512 {
		t.Errorf("memory = %d swap = %d pids = %v", *r.Memory.Limit, *r.Memory.Swap, r.Pids)
	}

	var netns string
	for _, ns := range s.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			netns = ns.Path
		}
	}
	if netns != files.Netns {
		t.Errorf("network namespace = %q", netns)
	}

	want := map[string]specs.Mount{
		"/workspace": {Destination: "/workspace", Type: "bind", Source: "/var/lib/pdd-sandbox/workspaces/run_0123", Options: []string{"rbind", "rw"}},
		"/cache":     {Destination: "/cache", Type: "bind", Source: "/var/lib/pdd-sandbox/cache/7/1", Options: []string{"rbind", "rw"}},
		"/ssl":       {Destination: "/ssl", Type: "bind", Source: "/etc/ssl", Options: []string{"rbind", "ro"}},
		"/tmp":       {Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"rw", "exec", "nosuid", "size=512m"}},
		"/run":       {Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"rw", "noexec"}},
		"/etc/hosts": {Destination: "/etc/hosts", Type: "bind", Source: files.Hosts, Options: []string{"rbind", "ro"}},
	}
	got := map[string]specs.Mount{}
	for _, m := range s.Mounts {
		got[m.Destination] = m
	}
	for dest, m := range want {
		if !reflect.DeepEqual(got[dest], m) {
			t.Errorf("mount %s = %+v, want %+v", dest, got[dest], m)
		}
	}
}

func TestSpecOptsOnlyApplyWhatIsSet(t *testing.T) {
	s := buildSpec(t, runtime.ContainerSpec{Name: "plain"}, sandboxFiles{})

	if s.Root.Readonly || s.Linux.Resources.CPU != nil || s.Linux.Resources.Memory != nil || s.Linux.Resources.Pids != nil {
		t.Errorf("unset options applied: readonly=%v resources=%+v", s.Root.Readonly, s.Linux.Resources)
	}
	if len(s.Process.Capabilities.Effective) == 0 {
		t.Error("default capabilities were dropped without CapDrop")
	}
	for _, m := range s.Mounts {
		if m.Destination == "/etc/hosts" {
			t.Errorf("hosts file mounted without a path: %+v", m)
		}
	}
	for _, ns := range s.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace && ns.Path != "" {
			t.Errorf("network namespace path = %q", ns.Path)
		}
	}
}

func TestEntrypointAndCmd(t *testing.T) {
	s := buildSpec(t, runtime.ContainerSpec{Name: "a", Entrypoint: []string{"sh", "./mvnw"}}, sandboxFiles{})
	if !reflect.DeepEqual(s.Process.Args, []string{"sh", "./mvnw"}) {
		t.Errorf("entrypoint only: %v", s.Process.Args)
	}
	s = buildSpec(t, runtime.ContainerSpec{Name: "a", Entrypoint: []string{"gradle"}, Cmd: []string{"test", "--no-daemon"}}, sandboxFiles{})
	if !reflect.DeepEqual(s.Process.Args, []string{"gradle", "test", "--no-daemon"}) {
		t.Errorf("entrypoint and cmd: %v", s.Process.Args)
	}
}

func TestCapDropKeepsTheOthers(t *testing.T) {
	spec := runtime.ContainerSpec{Name: "a", CapDrop: []string{"net_raw", "CAP_CHOWN"}}
	s := buildSpec(t, spec, sandboxFiles{})
	for _, set := range [][]string{s.Process.Capabilities.Bounding, s.Process.Capabilities.Effective, s.Process.Capabilities.Permitted} {
		joined := strings.Join(set, " ")
		if strings.Contains(joined, "CAP_NET_RAW") || strings.Contains(joined, "CAP_CHOWN") || !strings.Contains(joined, "CAP_KILL") {
			t.Errorf("capabilities = %v", set)
		}
	}
}

func TestSpecOptsRejectUnsupportedSecurityOption(t *testing.T) {
	for _, opt := range []string{"seccomp=unconfined", "apparmor=x", "no-new-privileges:false"} {
		if _, err := specOpts(runtime.ContainerSpec{SecurityOpt: []string{opt}}, nil, sandboxFiles{}); err == nil {
			t.Errorf("%q accepted", opt)
		}
	}
	if _, err := specOpts(runtime.ContainerSpec{SecurityOpt: []string{"no-new-privileges:true"}}, nil, sandboxFiles{}); err != nil {
		t.Error(err)
	}
}

func TestHostname(t *testing.T) {
	long := strings.Repeat("a", 80)
	if got := hostname(long); len(got) != 63 || hostname("short") != "short" {
		t.Errorf("hostname = %q", got)
	}
}
