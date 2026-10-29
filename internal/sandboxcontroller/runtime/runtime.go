// Package runtime defines the container runtime the reconcile loop drives. The containerd
// implementation lives in the containerd subpackage and a fake for tests in runtimetest.
package runtime

import (
	"context"
	"errors"
	"time"
)

// LabelRun is the container label that ties a container to its SandboxRun.
const LabelRun = "pdd.sandbox/run"

var (
	// ErrNotFound means the container (or image) does not exist.
	ErrNotFound = errors.New("runtime: not found")
	// ErrUnavailable means the container runtime cannot run sandboxes: the runtime cannot be
	// reached or the required OCI runtime (runsc) is not registered.
	ErrUnavailable = errors.New("runtime: unavailable")
)

// Mount is a bind mount.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// ContainerSpec describes the container of one run.
type ContainerSpec struct {
	Name       string
	Image      string
	Labels     map[string]string
	Entrypoint []string
	Cmd        []string
	Env        []string
	WorkingDir string
	User       string
	// Runtime is the OCI runtime, always runsc in production.
	Runtime string
	Network string
	Mounts  []Mount
	Tmpfs   map[string]string

	CPUs        float64
	MemoryBytes int64
	PidsLimit   int64

	CapDrop        []string
	SecurityOpt    []string
	ReadonlyRootfs bool
}

// State is the coarse state of a container.
type State string

// Container states.
const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateExited  State = "exited"
)

// Container is what the runtime reports about one container.
type Container struct {
	ID         string
	Name       string
	RunID      string
	State      State
	ExitCode   int
	OOMKilled  bool
	StartedAt  time.Time
	FinishedAt time.Time
}

// NetworkInfo describes the sandbox network.
type NetworkInfo struct {
	// Gateway is the address of the host on the network.
	Gateway string
}

// Runtime creates and observes sandbox containers. Every method must be safe for concurrent
// use, and Stop and Remove must succeed when the container is already gone.
type Runtime interface {
	// CheckRuntime returns ErrUnavailable unless the OCI runtime name is registered.
	CheckRuntime(ctx context.Context, name string) error
	// EnsureNetwork prepares the sandbox network when needed and describes it.
	EnsureNetwork(ctx context.Context, name string) (NetworkInfo, error)
	// Create creates a stopped container, or returns the id of the one with the same name.
	Create(ctx context.Context, spec ContainerSpec) (string, error)
	Start(ctx context.Context, id string) error
	// Inspect returns ErrNotFound for an unknown container.
	Inspect(ctx context.Context, id string) (Container, error)
	// Logs returns at most the last tailBytes bytes of stdout and stderr.
	Logs(ctx context.Context, id string, tailBytes int) (string, error)
	Stop(ctx context.Context, id string, grace time.Duration) error
	Remove(ctx context.Context, id string) error
	// List returns every container that has the LabelRun label, in any state.
	List(ctx context.Context) ([]Container, error)
}
