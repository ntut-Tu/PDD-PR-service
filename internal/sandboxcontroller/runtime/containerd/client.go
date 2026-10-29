// Package containerd implements runtime.Runtime on a containerd that runs next to the
// controller, usually in the same container.
//
// Sandboxes run in their own containerd namespace under gVisor: either the runc v2 shim with
// runsc as its runtime binary (the way Docker drives runsc) or the runsc shim. Each sandbox gets
// a network namespace of its own, connected by a CNI ptp veth pair without masquerade, so the
// only thing it can reach is the host end of that pair. Output goes to a file per container.
package containerd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/api/types/runc/options"
	ctrd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"

	"pdd/services/internal/sandboxcontroller/runtime"
)

// Shims a Config can select.
const (
	// ShimRunc runs sandboxes with io.containerd.runc.v2 and runsc as its runtime binary.
	ShimRunc = "runc"
	// ShimRunsc runs sandboxes with io.containerd.runsc.v1.
	ShimRunsc = "runsc"
)

const (
	callTimeout = 30 * time.Second
	dialTimeout = 3 * time.Second
	pullTimeout = 15 * time.Minute

	runcRuntimeType  = "io.containerd.runc.v2"
	runscRuntimeType = "io.containerd.runsc.v1"
	runscShimBinary  = "containerd-shim-runsc-v1"
	ociRunsc         = "runsc"
	ociRunc          = "runc"
	snapshotter      = "overlayfs"
)

// Config locates containerd and the files the runtime keeps. Every field is required.
type Config struct {
	// Address is the containerd socket.
	Address string
	// Namespace is the containerd namespace of the sandboxes.
	Namespace string
	// Shim is ShimRunc or ShimRunsc.
	Shim string
	// RunscPath is the runsc binary used with ShimRunc.
	RunscPath string
	// LogDir receives <container>.log and <container>.hosts.
	LogDir string
	// NetnsDir holds the network namespace files; it belongs on a tmpfs.
	NetnsDir string
	// CNIBinDir holds the ptp, host-local and loopback plugins.
	CNIBinDir string
	// CNIDataDir is where host-local keeps its leases; it belongs on a tmpfs, so that a restart
	// of the runtime container releases every address.
	CNIDataDir string
	// CNISubnet is the IPv4 subnet of the sandboxes. Its first address is the host end of
	// every veth pair.
	CNISubnet string
	// ProxyPort is the only TCP port of the runtime container the sandboxes may reach.
	ProxyPort int

	// ipForwardPath replaces the sysctl file, for tests.
	ipForwardPath string
}

func (c Config) validate() error {
	var errs []error
	for name, value := range map[string]string{
		"Address": c.Address, "Namespace": c.Namespace, "RunscPath": c.RunscPath, "LogDir": c.LogDir,
		"NetnsDir": c.NetnsDir, "CNIBinDir": c.CNIBinDir, "CNIDataDir": c.CNIDataDir, "CNISubnet": c.CNISubnet,
	} {
		if value == "" {
			errs = append(errs, fmt.Errorf("containerd: %s must not be empty", name))
		}
	}
	if c.Shim != ShimRunc && c.Shim != ShimRunsc {
		errs = append(errs, fmt.Errorf("containerd: shim %q must be %s or %s", c.Shim, ShimRunc, ShimRunsc))
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		errs = append(errs, fmt.Errorf("containerd: ProxyPort %d is not a port", c.ProxyPort))
	}
	if _, _, err := cniConfig("check", c.CNISubnet, c.CNIDataDir); err != nil && c.CNISubnet != "" {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Client drives one containerd. It connects lazily and reconnects by itself, so New succeeds
// while containerd is still starting; calls then fail with runtime.ErrUnavailable.
type Client struct {
	cfg    Config
	client *ctrd.Client

	run      runner
	lookPath func(string) (string, error)
	locks    keyedMutex
	pulls    singleflight.Group

	netMu    sync.Mutex
	networks map[string]*network
}

// New returns a client for the containerd at cfg.Address.
func New(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	client, err := ctrd.New(cfg.Address,
		ctrd.WithDefaultNamespace(cfg.Namespace),
		ctrd.WithTimeout(dialTimeout),
		ctrd.WithExtraDialOpts([]grpc.DialOption{grpc.WithContextDialer(dialSocket)}),
	)
	if err != nil {
		return nil, fmt.Errorf("containerd: %w", err)
	}
	return &Client{cfg: cfg, client: client, run: execRunner, lookPath: exec.LookPath, networks: map[string]*network{}}, nil
}

// dialSocket connects to the containerd socket. Unlike containerd's own dialer it does not wait
// for a missing socket until the deadline of the call, so calls fail at once while containerd is down.
func dialSocket(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "unix", strings.TrimPrefix(address, "unix://"))
}

// Close closes the connection to containerd.
func (c *Client) Close() error { return c.client.Close() }

// ctx adds the namespace to ctx, which the OCI spec generation reads from it.
func (c *Client) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, c.cfg.Namespace)
}

// ociRuntime is how containerd starts the containers of one OCI runtime name.
type ociRuntime struct {
	// Type is the containerd runtime (shim) name.
	Type string
	// Options is passed to the shim; nil for none.
	Options any
	// Binary is what has to be installed for the runtime to work.
	Binary string
}

// ociRuntime maps the name the reconcile loop asks for to a shim. "runsc" is the production
// runtime; "runc" lets tests run on hosts without gVisor.
func (c *Client) ociRuntime(name string) (ociRuntime, error) {
	switch {
	case name == ociRunsc && c.cfg.Shim == ShimRunsc:
		return ociRuntime{Type: runscRuntimeType, Binary: runscShimBinary}, nil
	case name == ociRunsc:
		return ociRuntime{Type: runcRuntimeType, Options: &options.Options{BinaryName: c.cfg.RunscPath}, Binary: c.cfg.RunscPath}, nil
	case name == ociRunc:
		return ociRuntime{Type: runcRuntimeType, Binary: ociRunc}, nil
	default:
		return ociRuntime{}, fmt.Errorf("%w: the OCI runtime %q is not supported by containerd", runtime.ErrUnavailable, name)
	}
}

// CheckRuntime implements runtime.Runtime. It reports whether containerd answers and the
// binary that runs the named OCI runtime is installed.
func (c *Client) CheckRuntime(ctx context.Context, name string) error {
	oci, err := c.ociRuntime(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := c.client.Version(c.ctx(ctx)); err != nil {
		return fmt.Errorf("%w: containerd at %s: %v", runtime.ErrUnavailable, c.cfg.Address, err)
	}
	if _, err := exec.LookPath(oci.Binary); err != nil {
		return fmt.Errorf("%w: the OCI runtime %q is not installed: %v", runtime.ErrUnavailable, name, err)
	}
	return nil
}

// wrap adds the operation to an error of containerd and maps an unreachable daemon to
// runtime.ErrUnavailable.
func wrap(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errdefs.IsUnavailable(err):
		return fmt.Errorf("%w: containerd %s: %v", runtime.ErrUnavailable, op, err)
	default:
		return fmt.Errorf("containerd: %s: %w", op, err)
	}
}

// keyedMutex serializes the operations on one container.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	users int
}

// lock blocks until the caller holds the lock of key and returns the function that releases it.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.users++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.users--; l.users == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
