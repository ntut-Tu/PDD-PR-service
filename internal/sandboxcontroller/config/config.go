// Package config loads and validates the sandbox-controller settings from hierarchical YAML.
//
// The embedded values.yaml holds every default. Files given with --config are deep-merged over
// it (maps merge, scalars and lists replace), the result is decoded strictly, and all problems
// are reported together, each naming its YAML path.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed values.yaml
var defaultValues []byte

// Config holds every setting of the service.
type Config struct {
	ListenAddr      string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	InternalToken   string
	DataDir         string

	Runtime    Runtime
	GitGateway GitGateway
	Workspace  Workspace
	CacheDir   string
	Limits     Limits
	Network    Network
	Images     Images

	OutputMaxBytes     int
	WorkspaceRetention time.Duration
	ResyncInterval     time.Duration
}

// Runtime configures the container runtime of the sandboxes.
type Runtime struct {
	Containerd ContainerdRuntime
}

// ContainerdRuntime configures the containerd runtime.
type ContainerdRuntime struct {
	Address    string
	Namespace  string
	Shim       string
	RunscPath  string
	LogDir     string
	NetnsDir   string
	CNIBinDir  string
	CNIDataDir string
	CNISubnet  string
}

// GitGateway configures the source archive client.
type GitGateway struct {
	URL           string
	InternalToken string
	Timeout       time.Duration
}

// Workspace configures the run checkouts.
type Workspace struct {
	Dir      string
	MaxBytes int64
	MaxFiles int
}

// Limits are the resource limits of the sandboxes.
type Limits struct {
	Timeout       time.Duration
	CPUs          float64
	MemoryBytes   int64
	Pids          int
	MaxConcurrent int
	User          string
}

// Network configures the sandbox network and its allow-list proxy.
type Network struct {
	Name         string
	AllowedHosts []string
	ProxyListen  string
	ProxyHost    string
}

// Images are the build images per build tool.
type Images struct {
	Maven  string
	Gradle string
}

// Load reads the files in order over the embedded defaults and returns the validated settings.
func Load(paths []string) (Config, error) {
	docs := make([]document, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		docs = append(docs, document{name: path, data: data})
	}
	return load(docs)
}

// LoadYAML is Load for configuration documents that are already in memory.
func LoadYAML(overlays ...[]byte) (Config, error) {
	docs := make([]document, 0, len(overlays))
	for i, data := range overlays {
		docs = append(docs, document{name: fmt.Sprintf("overlay %d", i+1), data: data})
	}
	return load(docs)
}

type document struct {
	name string
	data []byte
}

func load(overlays []document) (Config, error) {
	merged, err := parseMapping(document{name: "values.yaml", data: defaultValues})
	if err != nil {
		return Config{}, err
	}
	for _, doc := range overlays {
		overlay, err := parseMapping(doc)
		if err != nil {
			return Config{}, err
		}
		merged = deepMerge(merged, overlay)
	}
	var v values
	if err := newDecoder().decode(&v, merged); err != nil {
		return Config{}, err
	}
	return v.validate()
}

func parseMapping(doc document) (map[string]any, error) {
	var m map[string]any
	if err := yaml.Unmarshal(doc.data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", doc.name, err)
	}
	return m, nil
}

// deepMerge returns base with overlay applied: maps merge key by key, every other value
// (scalar, list) in overlay replaces the one in base. A null in overlay keeps the base value.
func deepMerge(base, overlay map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for key, over := range overlay {
		if over == nil {
			continue
		}
		overMap, overIsMap := over.(map[string]any)
		baseMap, baseIsMap := base[key].(map[string]any)
		if overIsMap && baseIsMap {
			base[key] = deepMerge(baseMap, overMap)
			continue
		}
		base[key] = over
	}
	return base
}

// values mirrors the layout of values.yaml.
type values struct {
	Server struct {
		ListenAddr      string        `yaml:"listenAddr"`
		LogLevel        string        `yaml:"logLevel"`
		ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	} `yaml:"server"`
	Security struct {
		InternalToken string `yaml:"internalToken"`
	} `yaml:"security"`
	GitGateway struct {
		URL           string        `yaml:"url"`
		InternalToken string        `yaml:"internalToken"`
		Timeout       time.Duration `yaml:"timeout"`
	} `yaml:"gitGateway"`
	Storage struct {
		DataDir string `yaml:"dataDir"`
	} `yaml:"storage"`
	Workspace struct {
		Dir      string `yaml:"dir"`
		MaxBytes int    `yaml:"maxBytes"`
		MaxFiles int    `yaml:"maxFiles"`
	} `yaml:"workspace"`
	Cache struct {
		Dir string `yaml:"dir"`
	} `yaml:"cache"`
	Limits struct {
		Timeout       time.Duration `yaml:"timeout"`
		CPUs          float64       `yaml:"cpus"`
		Memory        string        `yaml:"memory"`
		Pids          int           `yaml:"pids"`
		MaxConcurrent int           `yaml:"maxConcurrent"`
		User          string        `yaml:"user"`
	} `yaml:"limits"`
	Network struct {
		Name         string   `yaml:"name"`
		AllowedHosts []string `yaml:"allowedHosts"`
		Proxy        struct {
			Listen string `yaml:"listen"`
			Host   string `yaml:"host"`
		} `yaml:"proxy"`
	} `yaml:"network"`
	Images struct {
		Maven  string `yaml:"maven"`
		Gradle string `yaml:"gradle"`
	} `yaml:"images"`
	Output struct {
		MaxBytes int `yaml:"maxBytes"`
	} `yaml:"output"`
	Retention struct {
		Workspace time.Duration `yaml:"workspace"`
	} `yaml:"retention"`
	Reconcile struct {
		ResyncInterval time.Duration `yaml:"resyncInterval"`
	} `yaml:"reconcile"`
	Runtime struct {
		Containerd struct {
			Address   string `yaml:"address"`
			Namespace string `yaml:"namespace"`
			Shim      string `yaml:"shim"`
			RunscPath string `yaml:"runscPath"`
			LogDir    string `yaml:"logDir"`
			NetnsDir  string `yaml:"netnsDir"`
			CNI       struct {
				BinDir  string `yaml:"binDir"`
				DataDir string `yaml:"dataDir"`
				Subnet  string `yaml:"subnet"`
			} `yaml:"cni"`
		} `yaml:"containerd"`
	} `yaml:"runtime"`
}

var (
	userPattern    = regexp.MustCompile(`^[0-9]+(:[0-9]+)?$`)
	networkPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	hostPattern    = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	sizePattern    = regexp.MustCompile(`^([0-9]+)(Ki|Mi|Gi)?$`)
	// namespacePattern is the syntax containerd accepts for namespaces.
	namespacePattern = regexp.MustCompile(`^[A-Za-z0-9]+([._-][A-Za-z0-9]+)*$`)
)

const (
	minMemoryBytes = 64 << 20
	maxSubnetBits  = 28
)

// validate checks every value, reporting all problems together, and builds the Config.
func (v *values) validate() (Config, error) {
	c := &checker{}
	cfg := Config{
		ListenAddr:      c.nonBlank("server.listenAddr", v.Server.ListenAddr),
		LogLevel:        c.logLevel("server.logLevel", v.Server.LogLevel),
		ShutdownTimeout: c.duration("server.shutdownTimeout", v.Server.ShutdownTimeout),
		InternalToken:   c.required("security.internalToken", v.Security.InternalToken),
		DataDir:         c.nonBlank("storage.dataDir", v.Storage.DataDir),
		Runtime: Runtime{
			Containerd: ContainerdRuntime{
				Address:    c.absPath("runtime.containerd.address", v.Runtime.Containerd.Address),
				Namespace:  c.pattern("runtime.containerd.namespace", v.Runtime.Containerd.Namespace, namespacePattern, "a containerd namespace such as pdd-sandbox"),
				Shim:       c.oneOf("runtime.containerd.shim", v.Runtime.Containerd.Shim, "runc", "runsc"),
				RunscPath:  c.absPath("runtime.containerd.runscPath", v.Runtime.Containerd.RunscPath),
				LogDir:     c.absPath("runtime.containerd.logDir", v.Runtime.Containerd.LogDir),
				NetnsDir:   c.absPath("runtime.containerd.netnsDir", v.Runtime.Containerd.NetnsDir),
				CNIBinDir:  c.absPath("runtime.containerd.cni.binDir", v.Runtime.Containerd.CNI.BinDir),
				CNIDataDir: c.absPath("runtime.containerd.cni.dataDir", v.Runtime.Containerd.CNI.DataDir),
				CNISubnet:  c.subnet("runtime.containerd.cni.subnet", v.Runtime.Containerd.CNI.Subnet),
			},
		},
		GitGateway: GitGateway{
			URL:           c.httpURL("gitGateway.url", v.GitGateway.URL),
			InternalToken: c.required("gitGateway.internalToken", v.GitGateway.InternalToken),
			Timeout:       c.duration("gitGateway.timeout", v.GitGateway.Timeout),
		},
		Workspace: Workspace{
			Dir:      c.nonBlank("workspace.dir", v.Workspace.Dir),
			MaxBytes: int64(c.positive("workspace.maxBytes", v.Workspace.MaxBytes)),
			MaxFiles: c.positive("workspace.maxFiles", v.Workspace.MaxFiles),
		},
		CacheDir: c.nonBlank("cache.dir", v.Cache.Dir),
		Limits: Limits{
			Timeout:       c.duration("limits.timeout", v.Limits.Timeout),
			CPUs:          c.positiveFloat("limits.cpus", v.Limits.CPUs),
			MemoryBytes:   c.size("limits.memory", v.Limits.Memory, minMemoryBytes),
			Pids:          c.positive("limits.pids", v.Limits.Pids),
			MaxConcurrent: c.positive("limits.maxConcurrent", v.Limits.MaxConcurrent),
			User:          c.user("limits.user", v.Limits.User),
		},
		Network: Network{
			Name:         c.pattern("network.name", v.Network.Name, networkPattern, "a network name such as pdd-sandbox"),
			AllowedHosts: c.hosts("network.allowedHosts", v.Network.AllowedHosts),
			ProxyListen:  c.nonBlank("network.proxy.listen", v.Network.Proxy.Listen),
			ProxyHost:    strings.TrimSpace(v.Network.Proxy.Host),
		},
		Images: Images{
			Maven:  c.nonBlank("images.maven", v.Images.Maven),
			Gradle: c.nonBlank("images.gradle", v.Images.Gradle),
		},
		OutputMaxBytes:     c.positive("output.maxBytes", v.Output.MaxBytes),
		WorkspaceRetention: c.duration("retention.workspace", v.Retention.Workspace),
		ResyncInterval:     c.duration("reconcile.resyncInterval", v.Reconcile.ResyncInterval),
	}
	if err := c.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// checker validates values and remembers the problems it met.
type checker struct{ errs []error }

func (c *checker) err() error { return errors.Join(c.errs...) }

func (c *checker) fail(path, format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func (c *checker) nonBlank(path, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		c.fail(path, "must not be empty")
	}
	return v
}

func (c *checker) required(path, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		c.fail(path, "required")
	}
	return v
}

func (c *checker) httpURL(path, v string) string {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		c.fail(path, "must be an absolute http(s) URL")
	}
	return v
}

func (c *checker) positive(path string, n int) int {
	if n < 1 {
		c.fail(path, "must be greater than zero")
	}
	return n
}

func (c *checker) duration(path string, d time.Duration) time.Duration {
	if d <= 0 {
		c.fail(path, "must be a positive duration such as 30s")
	}
	return d
}

func (c *checker) logLevel(path, v string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(v)); err != nil {
		c.fail(path, "must be debug, info, warn or error")
	}
	return level
}

func (c *checker) positiveFloat(path string, f float64) float64 {
	if f <= 0 {
		c.fail(path, "must be greater than zero")
	}
	return f
}

func (c *checker) pattern(path, v string, re *regexp.Regexp, what string) string {
	v = strings.TrimSpace(v)
	if !re.MatchString(v) {
		c.fail(path, "must be %s", what)
	}
	return v
}

func (c *checker) user(path, v string) string {
	v = c.pattern(path, v, userPattern, "a numeric uid or uid:gid")
	if v == "0" || strings.HasPrefix(v, "0:") {
		c.fail(path, "must not be root")
	}
	return v
}

// size parses plain bytes or a Ki, Mi or Gi suffix.
func (c *checker) size(path, v string, min int64) int64 {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		c.fail(path, "must be a size such as 2Gi, 512Mi or 1048576")
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		c.fail(path, "size out of range")
		return 0
	}
	switch m[2] {
	case "Ki":
		n <<= 10
	case "Mi":
		n <<= 20
	case "Gi":
		n <<= 30
	}
	if n < min {
		c.fail(path, "must be at least %d bytes", min)
	}
	return n
}

func (c *checker) oneOf(path, v string, allowed ...string) string {
	v = strings.TrimSpace(v)
	if !slices.Contains(allowed, v) {
		c.fail(path, "must be one of %s", strings.Join(allowed, ", "))
	}
	return v
}

func (c *checker) absPath(path, v string) string {
	v = strings.TrimSpace(v)
	if !filepath.IsAbs(v) {
		c.fail(path, "must be an absolute path")
	}
	return filepath.Clean(v)
}

// subnet checks for an IPv4 CIDR with room for the gateway and some sandboxes.
func (c *checker) subnet(path, v string) string {
	v = strings.TrimSpace(v)
	prefix, err := netip.ParsePrefix(v)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() > maxSubnetBits {
		c.fail(path, "must be an IPv4 CIDR such as 10.213.0.0/16, at most /%d", maxSubnetBits)
	}
	return v
}

func (c *checker) hosts(path string, list []string) []string {
	if len(list) == 0 {
		c.fail(path, "must list at least one host")
	}
	out := make([]string, 0, len(list))
	for i, h := range list {
		h = strings.ToLower(strings.TrimSpace(h))
		if !hostPattern.MatchString(h) {
			c.fail(fmt.Sprintf("%s[%d]", path, i), "must be a host name such as repo.example.com or *.example.com")
			continue
		}
		out = append(out, h)
	}
	return out
}
