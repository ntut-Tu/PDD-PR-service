package containerd

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"pdd/services/internal/sandboxcontroller/runtime"
)

const (
	chainInput   = "PDD-SANDBOX-IN"
	chainForward = "PDD-SANDBOX-FWD"
	// vethPrefix is how the ptp plugin names the host end of a link; IPv6 has no sandbox subnet
	// to match, so the links are matched by name.
	vethPrefix = "veth+"
)

// chain is a chain that holds the rules of the sandbox network, entered from a built-in chain.
type chain struct {
	name   string
	parent string
	// jump are the matches of the rule in parent that sends packets to the chain.
	jump  []string
	rules [][]string
}

// firewallChains returns the chains of the fail-closed policy. Sandboxes may only open TCP
// connections to proxyPort of the runtime container, and nothing is forwarded from or to them,
// whatever net.ipv4.ip_forward says.
func firewallChains(subnet netip.Prefix, proxyPort int) []chain {
	s := subnet.String()
	return []chain{
		{name: chainInput, parent: "INPUT", jump: []string{"-s", s}, rules: [][]string{
			{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
			{"-p", "tcp", "--dport", strconv.Itoa(proxyPort), "-j", "ACCEPT"},
			{"-j", "DROP"},
		}},
		{name: chainForward, parent: "FORWARD", rules: [][]string{
			{"-s", s, "-j", "DROP"},
			{"-d", s, "-j", "DROP"},
		}},
	}
}

// firewallChainsV6 drops what the links of the sandboxes send to or through the runtime
// container over IPv6, where they only have link-local addresses.
func firewallChainsV6() []chain {
	return []chain{
		{name: chainInput, parent: "INPUT", jump: []string{"-i", vethPrefix}, rules: [][]string{{"-j", "DROP"}}},
		{name: chainForward, parent: "FORWARD", jump: []string{"-i", vethPrefix}, rules: [][]string{{"-j", "DROP"}}},
	}
}

// runner runs a command and returns its combined output.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ensureFirewall installs the policy; it is idempotent and safe to call before every setup.
// Without it no sandbox may be started, so any failure is runtime.ErrUnavailable.
func (c *Client) ensureFirewall(ctx context.Context) error {
	prefix, err := netip.ParsePrefix(c.cfg.CNISubnet)
	if err != nil {
		return err
	}
	if err := c.ensureChains(ctx, "iptables", firewallChains(prefix.Masked(), c.cfg.ProxyPort)); err != nil {
		return err
	}
	if _, err := os.Stat("/proc/net/if_inet6"); err != nil {
		return nil
	}
	return c.ensureChains(ctx, "ip6tables", firewallChainsV6())
}

func (c *Client) ensureChains(ctx context.Context, bin string, chains []chain) error {
	path, err := c.lookPath(bin)
	if err != nil {
		return fmt.Errorf("%w: %s is required to isolate the sandboxes: %v", runtime.ErrUnavailable, bin, err)
	}
	for _, ch := range chains {
		if err := c.ensureChain(ctx, path, ch); err != nil {
			return fmt.Errorf("%w: %s rules of %s: %v", runtime.ErrUnavailable, bin, ch.name, err)
		}
	}
	return nil
}

func (c *Client) ensureChain(ctx context.Context, bin string, ch chain) error {
	run := func(args ...string) ([]byte, error) {
		out, err := c.run(ctx, bin, append([]string{"-w", "5"}, args...)...)
		if err != nil {
			return out, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
	listing, err := run("-S", ch.name)
	if err != nil {
		if _, err := run("-N", ch.name); err != nil {
			return err
		}
		listing = nil
	}
	if !c.chainMatches(run, ch, listing) {
		// A rule is missing or left over from another configuration. The chain is rebuilt; this
		// only happens on the first call of a runtime container and when the port changes.
		if _, err := run("-F", ch.name); err != nil {
			return err
		}
		for _, rule := range ch.rules {
			if _, err := run(append([]string{"-A", ch.name}, rule...)...); err != nil {
				return err
			}
		}
	}
	jump := append(append([]string(nil), ch.jump...), "-j", ch.name)
	if _, err := run(append([]string{"-C", ch.parent}, jump...)...); err != nil {
		if _, err := run(append([]string{"-I", ch.parent, "1"}, jump...)...); err != nil {
			return err
		}
	}
	return nil
}

// chainMatches reports whether the chain holds exactly the wanted rules.
func (c *Client) chainMatches(run func(args ...string) ([]byte, error), ch chain, listing []byte) bool {
	appended := 0
	for _, line := range strings.Split(string(listing), "\n") {
		if strings.HasPrefix(line, "-A ") {
			appended++
		}
	}
	if appended != len(ch.rules) {
		return false
	}
	for _, rule := range ch.rules {
		if _, err := run(append([]string{"-C", ch.name}, rule...)...); err != nil {
			return false
		}
	}
	return true
}
