package containerd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"pdd/services/internal/sandboxcontroller/runtime"
)

const (
	cpuPeriod  = 100_000
	hostsPath  = "/etc/hosts"
	maxHostLen = 63
)

// sandboxFiles are the host files and the namespace a container is wired to.
type sandboxFiles struct {
	Netns string
	Hosts string
}

// specOpts translates a ContainerSpec into OCI spec options. image supplies the defaults of the
// image (environment, user, working directory, command); a nil image means none.
func specOpts(spec runtime.ContainerSpec, image oci.Image, files sandboxFiles) ([]oci.SpecOpts, error) {
	var opts []oci.SpecOpts
	switch {
	case image == nil:
	case len(spec.Entrypoint) == 0:
		opts = append(opts, oci.WithImageConfigArgs(image, spec.Cmd))
	default:
		opts = append(opts, oci.WithImageConfig(image))
	}
	if len(spec.Entrypoint) > 0 {
		opts = append(opts, withProcessArgs(spec.Entrypoint, spec.Cmd))
	}
	if spec.User != "" {
		opts = append(opts, oci.WithUser(spec.User))
	}
	if len(spec.Env) > 0 {
		opts = append(opts, oci.WithEnv(spec.Env))
	}
	if spec.WorkingDir != "" {
		opts = append(opts, oci.WithProcessCwd(spec.WorkingDir))
	}
	opts = append(opts, oci.WithHostname(hostname(spec.Name)), withMounts(spec, files))
	if spec.ReadonlyRootfs {
		opts = append(opts, oci.WithRootFSReadonly())
	}
	for _, opt := range spec.SecurityOpt {
		if opt != "no-new-privileges" && opt != "no-new-privileges:true" {
			return nil, fmt.Errorf("containerd: unsupported security option %q", opt)
		}
		opts = append(opts, oci.WithNoNewPrivileges)
	}
	if len(spec.CapDrop) > 0 {
		opts = append(opts, withCapDrop(spec.CapDrop))
	}
	if spec.CPUs > 0 {
		opts = append(opts, oci.WithCPUCFS(int64(spec.CPUs*cpuPeriod), cpuPeriod))
	}
	if spec.MemoryBytes > 0 {
		// Swap equal to memory means none: the OCI value is memory plus swap.
		opts = append(opts, oci.WithMemoryLimit(uint64(spec.MemoryBytes)), oci.WithMemorySwap(spec.MemoryBytes))
	}
	if spec.PidsLimit > 0 {
		opts = append(opts, oci.WithPidsLimit(spec.PidsLimit))
	}
	if files.Netns != "" {
		opts = append(opts, oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: files.Netns}))
	}
	return opts, nil
}

func hostname(name string) string {
	if len(name) > maxHostLen {
		return name[:maxHostLen]
	}
	return name
}

// withProcessArgs replaces the command line of the image: like Docker, an entrypoint given at
// creation also drops the default command of the image.
func withProcessArgs(entrypoint, cmd []string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.Args = append(append([]string(nil), entrypoint...), cmd...)
		return nil
	}
}

// withMounts adds the bind mounts, the tmpfs mounts and the generated /etc/hosts.
func withMounts(spec runtime.ContainerSpec, files sandboxFiles) oci.SpecOpts {
	var mounts []specs.Mount
	for _, m := range spec.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		mounts = append(mounts, specs.Mount{Destination: m.Target, Type: "bind", Source: m.Source, Options: []string{"rbind", mode}})
	}
	targets := make([]string, 0, len(spec.Tmpfs))
	for target := range spec.Tmpfs {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		var options []string
		for _, option := range strings.Split(spec.Tmpfs[target], ",") {
			if option = strings.TrimSpace(option); option != "" {
				options = append(options, option)
			}
		}
		mounts = append(mounts, specs.Mount{Destination: target, Type: "tmpfs", Source: "tmpfs", Options: options})
	}
	if files.Hosts != "" {
		mounts = append(mounts, specs.Mount{Destination: hostsPath, Type: "bind", Source: files.Hosts, Options: []string{"rbind", "ro"}})
	}
	return oci.WithMounts(mounts)
}

// withCapDrop removes capabilities from every set of the process; "ALL" removes them all.
func withCapDrop(drop []string) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Process == nil || s.Process.Capabilities == nil {
			return nil
		}
		caps := s.Process.Capabilities
		sets := []*[]string{&caps.Bounding, &caps.Effective, &caps.Permitted, &caps.Inheritable, &caps.Ambient}
		for _, name := range drop {
			name = strings.TrimPrefix(strings.ToUpper(name), "CAP_")
			for _, set := range sets {
				*set = filterCaps(*set, name)
			}
		}
		return nil
	}
}

func filterCaps(set []string, drop string) []string {
	var kept []string
	for _, c := range set {
		if drop != "ALL" && c != "CAP_"+drop {
			kept = append(kept, c)
		}
	}
	return kept
}
