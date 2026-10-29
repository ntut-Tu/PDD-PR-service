// Package config loads and validates the git-gateway settings from hierarchical YAML.
//
// The embedded values.yaml holds every default. Files given with --config are deep-merged over
// it (maps merge, scalars and lists replace), the result is decoded strictly, and all problems
// are reported together, each naming its YAML path.
package config

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const aesKeyBytes = 32

//go:embed values.yaml
var defaultValues []byte

// Config holds every setting of the service.
type Config struct {
	ListenAddr      string
	DataDir         string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration

	JWTSecret     string
	InternalToken string
	// TokenKey is the decoded AES-256 key that encrypts provider tokens at rest.
	TokenKey []byte

	Backend  Backend
	Provider Provider
	Upstream Upstream
	Cache    Cache
	Files    Files
	Archive  Archive
}

// Backend configures the PDD backend client.
type Backend struct {
	URL         string
	InsecureTLS bool
	Timeout     time.Duration
	CacheTTL    time.Duration
}

// Provider configures the Git providers.
type Provider struct {
	GitHubAPIURL  string
	ListBatchSize int
	ListMaxPages  int
}

// Upstream configures the request policy of provider calls.
type Upstream struct {
	ReserveFraction   float64
	ReserveMin        int
	ExpectedCost      int
	Concurrency       int
	RetryMaxAttempts  int
	RetryBaseDelay    time.Duration
	RetryMaxDelay     time.Duration
	RetryMaxRetryWait time.Duration
	Timeout           time.Duration
	MaxBodyBytes      int64
}

// Cache configures the lifetimes of cached provider data.
type Cache struct {
	TTLPullList   time.Duration
	TTLPullDetail time.Duration
	TTLTree       time.Duration
	TTLFile       time.Duration
	StaleTTL      time.Duration
}

// Files configures the repository content endpoints.
type Files struct {
	MaxBytes int
	MaxPages int
}

// Archive configures the source archive endpoint.
type Archive struct {
	// MaxBytes is the largest accepted archive.
	MaxBytes int64
	// CacheMaxBytes is the total size of the archives kept on disk.
	CacheMaxBytes int64
	// Timeout bounds one archive download, including its body.
	Timeout time.Duration
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
	PDD struct {
		JWTSecret string `yaml:"jwtSecret"`
		Backend   struct {
			URL         string        `yaml:"url"`
			InsecureTLS bool          `yaml:"insecureTLS"`
			Timeout     time.Duration `yaml:"timeout"`
			CacheTTL    time.Duration `yaml:"cacheTTL"`
		} `yaml:"backend"`
	} `yaml:"pdd"`
	Security struct {
		TokenKey      string `yaml:"tokenKey"`
		InternalToken string `yaml:"internalToken"`
	} `yaml:"security"`
	Storage struct {
		DataDir string `yaml:"dataDir"`
	} `yaml:"storage"`
	GitHub struct {
		APIURL string `yaml:"apiURL"`
	} `yaml:"github"`
	Upstream struct {
		Timeout      time.Duration `yaml:"timeout"`
		MaxBodyBytes int64         `yaml:"maxBodyBytes"`
		Concurrency  int           `yaml:"concurrency"`
	} `yaml:"upstream"`
	RateLimit struct {
		ReserveFraction     float64 `yaml:"reserveFraction"`
		ReserveMin          int     `yaml:"reserveMin"`
		GraphQLExpectedCost int     `yaml:"graphqlExpectedCost"`
	} `yaml:"rateLimit"`
	Retry struct {
		MaxAttempts   int           `yaml:"maxAttempts"`
		BaseDelay     time.Duration `yaml:"baseDelay"`
		MaxDelay      time.Duration `yaml:"maxDelay"`
		MaxRetryAfter time.Duration `yaml:"maxRetryAfter"`
	} `yaml:"retry"`
	Cache struct {
		TTL struct {
			PRList   time.Duration `yaml:"prList"`
			PRDetail time.Duration `yaml:"prDetail"`
			Tree     time.Duration `yaml:"tree"`
			File     time.Duration `yaml:"file"`
		} `yaml:"ttl"`
		StaleTTL time.Duration `yaml:"staleTTL"`
	} `yaml:"cache"`
	Files struct {
		MaxBytes int `yaml:"maxBytes"`
		MaxPages int `yaml:"maxPages"`
	} `yaml:"files"`
	Archive struct {
		MaxBytes      int64         `yaml:"maxBytes"`
		CacheMaxBytes int64         `yaml:"cacheMaxBytes"`
		Timeout       time.Duration `yaml:"timeout"`
	} `yaml:"archive"`
	List struct {
		BatchSize int `yaml:"batchSize"`
		MaxPages  int `yaml:"maxPages"`
	} `yaml:"list"`
}

// validate checks every value, reporting all problems together, and builds the Config.
func (v *values) validate() (Config, error) {
	c := &checker{}
	cfg := Config{
		ListenAddr:      c.nonBlank("server.listenAddr", v.Server.ListenAddr),
		DataDir:         c.nonBlank("storage.dataDir", v.Storage.DataDir),
		LogLevel:        c.logLevel("server.logLevel", v.Server.LogLevel),
		ShutdownTimeout: c.duration("server.shutdownTimeout", v.Server.ShutdownTimeout),
		JWTSecret:       c.required("pdd.jwtSecret", v.PDD.JWTSecret),
		InternalToken:   c.required("security.internalToken", v.Security.InternalToken),
		TokenKey:        c.tokenKey("security.tokenKey", v.Security.TokenKey),
		Backend: Backend{
			URL:         c.httpURL("pdd.backend.url", v.PDD.Backend.URL),
			InsecureTLS: v.PDD.Backend.InsecureTLS,
			Timeout:     c.duration("pdd.backend.timeout", v.PDD.Backend.Timeout),
			CacheTTL:    c.duration("pdd.backend.cacheTTL", v.PDD.Backend.CacheTTL),
		},
		Provider: Provider{
			GitHubAPIURL:  c.httpURL("github.apiURL", v.GitHub.APIURL),
			ListBatchSize: c.positive("list.batchSize", v.List.BatchSize),
			ListMaxPages:  c.positive("list.maxPages", v.List.MaxPages),
		},
		Upstream: Upstream{
			ReserveFraction:   c.fraction("rateLimit.reserveFraction", v.RateLimit.ReserveFraction),
			ReserveMin:        c.nonNegative("rateLimit.reserveMin", v.RateLimit.ReserveMin),
			ExpectedCost:      c.positive("rateLimit.graphqlExpectedCost", v.RateLimit.GraphQLExpectedCost),
			Concurrency:       c.positive("upstream.concurrency", v.Upstream.Concurrency),
			RetryMaxAttempts:  c.positive("retry.maxAttempts", v.Retry.MaxAttempts),
			RetryBaseDelay:    c.duration("retry.baseDelay", v.Retry.BaseDelay),
			RetryMaxDelay:     c.duration("retry.maxDelay", v.Retry.MaxDelay),
			RetryMaxRetryWait: c.duration("retry.maxRetryAfter", v.Retry.MaxRetryAfter),
			Timeout:           c.duration("upstream.timeout", v.Upstream.Timeout),
			MaxBodyBytes:      int64(c.positive("upstream.maxBodyBytes", int(v.Upstream.MaxBodyBytes))),
		},
		Cache: Cache{
			TTLPullList:   c.duration("cache.ttl.prList", v.Cache.TTL.PRList),
			TTLPullDetail: c.duration("cache.ttl.prDetail", v.Cache.TTL.PRDetail),
			TTLTree:       c.duration("cache.ttl.tree", v.Cache.TTL.Tree),
			TTLFile:       c.duration("cache.ttl.file", v.Cache.TTL.File),
			StaleTTL:      c.duration("cache.staleTTL", v.Cache.StaleTTL),
		},
		Files: Files{
			MaxBytes: c.positive("files.maxBytes", v.Files.MaxBytes),
			MaxPages: c.positive("files.maxPages", v.Files.MaxPages),
		},
		Archive: Archive{
			MaxBytes:      c.positiveBytes("archive.maxBytes", v.Archive.MaxBytes),
			CacheMaxBytes: c.positiveBytes("archive.cacheMaxBytes", v.Archive.CacheMaxBytes),
			Timeout:       c.duration("archive.timeout", v.Archive.Timeout),
		},
	}
	if cfg.Archive.MaxBytes > cfg.Archive.CacheMaxBytes && cfg.Archive.CacheMaxBytes > 0 {
		c.fail("archive.cacheMaxBytes", "must not be smaller than archive.maxBytes")
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

func (c *checker) tokenKey(path, v string) []byte {
	v = c.required(path, v)
	if v == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(key) != aesKeyBytes {
		c.fail(path, "must be base64 of exactly %d bytes", aesKeyBytes)
		return nil
	}
	return key
}

func (c *checker) positive(path string, n int) int {
	if n < 1 {
		c.fail(path, "must be greater than zero")
	}
	return n
}

func (c *checker) positiveBytes(path string, n int64) int64 {
	if n < 1 {
		c.fail(path, "must be greater than zero")
	}
	return n
}

func (c *checker) nonNegative(path string, n int) int {
	if n < 0 {
		c.fail(path, "must not be negative")
	}
	return n
}

func (c *checker) fraction(path string, f float64) float64 {
	if f < 0 || f >= 1 {
		c.fail(path, "must be a number in [0, 1)")
	}
	return f
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
