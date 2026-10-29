package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/pkg/netns"
	gocni "github.com/containerd/go-cni"

	"pdd/services/internal/sandboxcontroller/runtime"
)

const (
	ipForwardPath = "/proc/sys/net/ipv4/ip_forward"
	// maxSubnetBits leaves room for the gateway and at least a few sandboxes.
	maxSubnetBits = 28
	cniVersion    = "1.0.0"
	ifName        = "eth0"
)

var cniPlugins = []string{"ptp", "host-local", "loopback"}

// network is one CNI network: a ptp veth pair per sandbox with host-local addresses and no
// masquerade, so a sandbox can reach the host side of its veth and nothing else.
type network struct {
	cni     gocni.CNI
	gateway netip.Addr
	// mu serializes setups. The ptp plugin switches net.ipv4.ip_forward on, so every setup is
	// followed by switching it off again before the next one starts.
	mu sync.Mutex
}

// attachment is what a sandbox gets from the network.
type attachment struct {
	netns string
	ip    netip.Addr
}

// cniConfig renders the conflist of a network and returns the address of the host side of the
// veth pairs. ptp gives the host end of every veth the gateway address of the subnet, which is
// its first usable address, so the sandboxes reach the services of the runtime container there.
func cniConfig(name, subnet, dataDir string) ([]byte, netip.Addr, error) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil || !prefix.Addr().Is4() {
		return nil, netip.Addr{}, fmt.Errorf("containerd: subnet %q is not an IPv4 CIDR", subnet)
	}
	if prefix.Bits() > maxSubnetBits {
		return nil, netip.Addr{}, fmt.Errorf("containerd: subnet %q is smaller than /%d", subnet, maxSubnetBits)
	}
	prefix = prefix.Masked()
	gateway := prefix.Addr().Next()
	conf := map[string]any{
		"cniVersion": cniVersion,
		"name":       name,
		"plugins": []any{map[string]any{
			"type":   "ptp",
			"ipMasq": false,
			"ipam": map[string]any{
				"type":    "host-local",
				"dataDir": dataDir,
				"ranges":  [][]map[string]string{{{"subnet": prefix.String(), "gateway": gateway.String()}}},
			},
		}},
	}
	data, err := json.Marshal(conf)
	if err != nil {
		return nil, netip.Addr{}, fmt.Errorf("containerd: encode CNI config: %w", err)
	}
	return data, gateway, nil
}

// network returns the CNI network with the given name, creating it on first use.
func (c *Client) network(name string) (*network, error) {
	c.netMu.Lock()
	defer c.netMu.Unlock()
	if n, ok := c.networks[name]; ok {
		return n, nil
	}
	for _, plugin := range cniPlugins {
		if err := checkExecutable(filepath.Join(c.cfg.CNIBinDir, plugin)); err != nil {
			return nil, fmt.Errorf("%w: CNI plugin %s: %v", runtime.ErrUnavailable, plugin, err)
		}
	}
	conf, gateway, err := cniConfig(name, c.cfg.CNISubnet, c.cfg.CNIDataDir)
	if err != nil {
		return nil, err
	}
	cni, err := gocni.New(
		gocni.WithPluginDir([]string{c.cfg.CNIBinDir}),
		gocni.WithInterfacePrefix("eth"),
		gocni.WithConfListBytes(conf),
		gocni.WithLoNetwork,
		gocni.WithMinNetworkCount(2),
	)
	if err != nil {
		return nil, fmt.Errorf("containerd: load CNI config: %w", err)
	}
	n := &network{cni: cni, gateway: gateway}
	c.networks[name] = n
	return n, nil
}

// EnsureNetwork implements runtime.Runtime. The sandbox network needs no setup of its own:
// every sandbox gets its own namespace when it is created. What the call guarantees is that the
// plugins are installed and that the sandboxes are fenced in: iptables rules drop everything they
// forward and everything they send to the runtime container except TCP to the proxy port, and
// IPv4 forwarding is off.
func (c *Client) EnsureNetwork(ctx context.Context, name string) (runtime.NetworkInfo, error) {
	n, err := c.network(name)
	if err != nil {
		return runtime.NetworkInfo{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := c.secure(ctx); err != nil {
		return runtime.NetworkInfo{}, err
	}
	return runtime.NetworkInfo{Gateway: n.gateway.String()}, nil
}

// secure installs the firewall policy and switches IPv4 forwarding off. Both must hold before a
// sandbox exists; the firewall is the guarantee, forwarding off a second layer.
func (c *Client) secure(ctx context.Context) error {
	if err := c.ensureFirewall(ctx); err != nil {
		return err
	}
	return c.disableForwarding()
}

// disableForwarding makes sure IPv4 forwarding is off in the network namespace of this process.
func (c *Client) disableForwarding() error {
	path := c.cfg.ipForwardPath
	if path == "" {
		path = ipForwardPath
	}
	read := func() (string, error) {
		data, err := os.ReadFile(path)
		return strings.TrimSpace(string(data)), err
	}
	value, err := read()
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", runtime.ErrUnavailable, path, err)
	}
	if value == "0" {
		return nil
	}
	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		return fmt.Errorf("%w: IPv4 forwarding is on and cannot be switched off: %v", runtime.ErrUnavailable, err)
	}
	if value, err = read(); err != nil || value != "0" {
		return fmt.Errorf("%w: IPv4 forwarding is still on after writing %s", runtime.ErrUnavailable, path)
	}
	return nil
}

// attach creates the network namespace of a sandbox and connects it to the network.
func (c *Client) attach(ctx context.Context, name, id string) (attachment, error) {
	n, err := c.network(name)
	if err != nil {
		return attachment{}, err
	}
	if err := os.MkdirAll(c.cfg.CNIDataDir, 0o700); err != nil {
		return attachment{}, fmt.Errorf("containerd: %w", err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := c.secure(ctx); err != nil {
		return attachment{}, err
	}
	ns, err := netns.NewNetNS(c.cfg.NetnsDir)
	if err != nil {
		return attachment{}, fmt.Errorf("containerd: create network namespace: %w", err)
	}
	result, err := n.cni.Setup(ctx, id, ns.GetPath())
	if err == nil {
		err = c.disableForwarding()
	}
	if err != nil {
		_ = c.detachNetwork(context.WithoutCancel(ctx), n, id, ns.GetPath())
		return attachment{}, fmt.Errorf("containerd: set up network of %s: %w", id, err)
	}
	cfg := result.Interfaces[ifName]
	if cfg == nil || len(cfg.IPConfigs) == 0 {
		_ = c.detachNetwork(context.WithoutCancel(ctx), n, id, ns.GetPath())
		return attachment{}, fmt.Errorf("containerd: CNI returned no address for %s", id)
	}
	ip, _ := netip.AddrFromSlice(cfg.IPConfigs[0].IP)
	return attachment{netns: ns.GetPath(), ip: ip.Unmap()}, nil
}

// detach releases the address and removes the network namespace of a sandbox. It succeeds when
// both are already gone; the namespace file of a previous run of the runtime container is a plain
// file without a namespace behind it, and CNI still has to release the address then.
func (c *Client) detach(ctx context.Context, name, id, nsPath string) error {
	if nsPath == "" {
		return nil
	}
	n, err := c.network(name)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return c.detachNetwork(ctx, n, id, nsPath)
}

func (c *Client) detachNetwork(ctx context.Context, n *network, id, nsPath string) error {
	errs := []error{n.cni.Remove(ctx, id, nsPath)}
	if filepath.Dir(nsPath) == filepath.Clean(c.cfg.NetnsDir) {
		errs = append(errs, removeNetNS(nsPath))
	}
	return errors.Join(errs...)
}

// removeNetNS unmounts and deletes a namespace file. A file left by an earlier run of the
// runtime container has no namespace behind it and is only deleted.
func removeNetNS(path string) error {
	ns := netns.LoadNetNS(path)
	if gone, err := ns.Closed(); err != nil || gone {
		return err
	}
	return ns.Remove()
}

// checkExecutable reports whether path is a regular file that can be executed.
func checkExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", path)
	}
	return nil
}

// hostsFile renders /etc/hosts of a sandbox; without it nothing resolves "localhost".
func hostsFile(hostname string, ip netip.Addr) []byte {
	var b bytes.Buffer
	b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	if ip.IsValid() {
		fmt.Fprintf(&b, "%s\t%s\n", ip, hostname)
	}
	return b.Bytes()
}
