package config_test

import (
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/config"
)

const requiredYAML = `
pdd: {jwtSecret: s}
security: {tokenKey: ` + "\"" + `AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=` + "\"" + `, internalToken: i}
`

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

	if cfg.ListenAddr != ":8080" || cfg.DataDir != "/data" || cfg.LogLevel != slog.LevelInfo || cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("server defaults = %+v", cfg)
	}
	if cfg.Backend.URL != "https://pdd-backend:5005" || cfg.Backend.InsecureTLS ||
		cfg.Backend.Timeout != 10*time.Second || cfg.Backend.CacheTTL != 30*time.Second {
		t.Errorf("backend defaults = %+v", cfg.Backend)
	}
	if cfg.Provider.GitHubAPIURL != "https://api.github.com" || cfg.Provider.ListBatchSize != 10 || cfg.Provider.ListMaxPages != 3 {
		t.Errorf("provider defaults = %+v", cfg.Provider)
	}
	want := config.Upstream{
		ReserveFraction: 0.10, ReserveMin: 10, ExpectedCost: 1, Concurrency: 4,
		RetryMaxAttempts: 3, RetryBaseDelay: 500 * time.Millisecond, RetryMaxDelay: 8 * time.Second,
		RetryMaxRetryWait: 30 * time.Second, Timeout: 30 * time.Second, MaxBodyBytes: 10485760,
	}
	if cfg.Upstream != want {
		t.Errorf("upstream defaults = %+v", cfg.Upstream)
	}
	wantCache := config.Cache{
		TTLPullList: time.Minute, TTLPullDetail: 2 * time.Minute, TTLTree: 5 * time.Minute,
		TTLFile: 10 * time.Minute, StaleTTL: 24 * time.Hour,
	}
	if cfg.Cache != wantCache {
		t.Errorf("cache defaults = %+v", cfg.Cache)
	}
	if cfg.Files != (config.Files{MaxBytes: 1048576, MaxPages: 20}) {
		t.Errorf("files defaults = %+v", cfg.Files)
	}
	if cfg.Archive != (config.Archive{MaxBytes: 209715200, CacheMaxBytes: 2147483648, Timeout: 5 * time.Minute}) {
		t.Errorf("archive defaults = %+v", cfg.Archive)
	}
	if cfg.JWTSecret != "s" || cfg.InternalToken != "i" || len(cfg.TokenKey) != 32 {
		t.Errorf("secrets = %+v", cfg)
	}
}

func TestEmbeddedValuesDocumentEveryKey(t *testing.T) {
	data, err := os.ReadFile("values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)

	for _, key := range []string{
		"listenAddr:", "logLevel:", "shutdownTimeout:", "jwtSecret:", "insecureTLS:", "cacheTTL:", "tokenKey:", "internalToken:",
		"dataDir:", "apiURL:", "maxBodyBytes:", "concurrency:", "reserveFraction:", "reserveMin:", "graphqlExpectedCost:",
		"maxAttempts:", "baseDelay:", "maxDelay:", "maxRetryAfter:", "prList:", "prDetail:", "tree:", "file:", "staleTTL:", "batchSize:", "maxPages:", "maxBytes:", "cacheMaxBytes:", "timeout:",
	} {
		loc := regexp.MustCompile(`(?m)^ *` + regexp.QuoteMeta(key)).FindStringIndex(text)
		if loc == nil {
			t.Errorf("values.yaml lacks %s", key)
			continue
		}
		before := strings.TrimRight(text[:loc[0]], "\n")
		lastLine := before[strings.LastIndex(before, "\n")+1:]
		if !strings.HasPrefix(strings.TrimSpace(lastLine), "#") {
			t.Errorf("%s has no comment above it", key)
		}
	}
}

func TestLoadDeepMergesFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first := write("a.yaml", requiredYAML)
	second := write("b.yaml", "pdd:\n  backend:\n    url: http://one:1\n    insecureTLS: true\nretry:\n  maxAttempts: 5\n")
	third := write("c.yaml", "pdd:\n  backend:\n    url: http://two:2/\ncache:\n  ttl:\n    tree: 1m\n")

	cfg, err := config.Load([]string{first, second, third})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Backend.URL != "http://two:2" {
		t.Errorf("later file must replace the scalar, URL = %q", cfg.Backend.URL)
	}
	if !cfg.Backend.InsecureTLS || cfg.Backend.Timeout != 10*time.Second {
		t.Errorf("sibling keys must survive the merge: %+v", cfg.Backend)
	}
	if cfg.JWTSecret != "s" || cfg.Upstream.RetryMaxAttempts != 5 {
		t.Errorf("first file lost: %+v", cfg)
	}
	if cfg.Cache.TTLTree != time.Minute || cfg.Cache.TTLFile != 10*time.Minute || cfg.Cache.TTLPullList != time.Minute {
		t.Errorf("cache = %+v", cfg.Cache)
	}
}

func TestLoadNullAndEmptyOverlayKeepDefaults(t *testing.T) {
	cfg := mustLoad(t, requiredYAML, "", "retry:\n  maxAttempts:\n")

	if cfg.Upstream.RetryMaxAttempts != 3 {
		t.Errorf("maxAttempts = %d", cfg.Upstream.RetryMaxAttempts)
	}
}

func TestLoadRejectsUnknownKeysWithPath(t *testing.T) {
	_, err := config.LoadYAML([]byte(requiredYAML + "retry:\n  maxAtempts: 5\nbogus: 1\nserver:\n  extra:\n    deep: 1\n"))

	if err == nil {
		t.Fatal("unknown keys accepted")
	}
	for _, path := range []string{"retry.maxAtempts: unknown key", "bogus: unknown key", "server.extra: unknown key"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error lacks %q:\n%v", path, err)
		}
	}
}

func TestLoadRejectsInvalidDurations(t *testing.T) {
	_, err := config.LoadYAML([]byte(requiredYAML + "cache:\n  ttl:\n    prList: soon\n    tree: 30\n"))

	if err == nil {
		t.Fatal("invalid durations accepted")
	}
	for _, path := range []string{"cache.ttl.prList: invalid duration", "cache.ttl.tree: must be a duration string"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error lacks %q:\n%v", path, err)
		}
	}
}

func TestLoadRejectsNonPositiveDurations(t *testing.T) {
	_, err := config.LoadYAML([]byte(requiredYAML), []byte("retry:\n  baseDelay: -5s\n"))

	if err == nil || !strings.Contains(err.Error(), "retry.baseDelay: must be a positive duration") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsWrongTypes(t *testing.T) {
	_, err := config.LoadYAML([]byte(
		"upstream: 5\nretry:\n  maxAttempts: many\n  baseDelay: {a: 1}\npdd:\n  backend:\n    insecureTLS: maybe\nserver:\n  listenAddr: 80\nrateLimit:\n  reserveFraction: high\n"))

	if err == nil {
		t.Fatal("wrong types accepted")
	}
	for _, path := range []string{
		"upstream: must be a mapping", "retry.maxAttempts: must be an integer", "retry.baseDelay: must be a single value",
		"pdd.backend.insecureTLS: must be true or false", "server.listenAddr: must be a string", "rateLimit.reserveFraction: must be a number",
	} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error lacks %q:\n%v", path, err)
		}
	}
}

func TestLoadReportsMalformedYAMLWithFileName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte("pdd: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := config.Load([]string{path})

	if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := config.Load([]string{filepath.Join(t.TempDir(), "nope.yaml")}); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestLoadReportsAllRequiredProblemsTogether(t *testing.T) {
	_, err := config.LoadYAML()

	if err == nil {
		t.Fatal("Load succeeded without secrets")
	}
	for _, line := range []string{"pdd.jwtSecret: required", "security.tokenKey: required", "security.internalToken: required"} {
		if !strings.Contains(err.Error(), line) {
			t.Errorf("error lacks %q:\n%v", line, err)
		}
	}
}

func TestLoadReportsAllValueProblemsTogether(t *testing.T) {
	_, err := config.LoadYAML([]byte(`
security: {tokenKey: "not base64!"}
pdd: {backend: {url: pdd-backend}}
upstream: {concurrency: 0}
rateLimit: {reserveFraction: 1.5, reserveMin: -1}
server: {logLevel: loud}
list: {batchSize: 0}
`))

	if err == nil {
		t.Fatal("Load succeeded")
	}
	for _, path := range []string{
		"pdd.jwtSecret", "security.internalToken", "security.tokenKey: must be base64", "pdd.backend.url: must be an absolute",
		"upstream.concurrency: must be greater", "rateLimit.reserveFraction: must be a number in", "rateLimit.reserveMin: must not be negative",
		"server.logLevel: must be debug", "list.batchSize: must be greater",
	} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error lacks %q:\n%v", path, err)
		}
	}
}

func TestLoadRejectsWrongKeyLength(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))

	_, err := config.LoadYAML([]byte(requiredYAML), []byte("security:\n  tokenKey: "+short+"\n"))

	if err == nil || !strings.Contains(err.Error(), "security.tokenKey") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadTreatsBlankSecretAsMissing(t *testing.T) {
	_, err := config.LoadYAML([]byte(requiredYAML), []byte("pdd:\n  jwtSecret: \"  \"\n"))

	if err == nil || !strings.Contains(err.Error(), "pdd.jwtSecret: required") {
		t.Fatalf("blank secret accepted: %v", err)
	}
}

func TestOverridesApply(t *testing.T) {
	cfg := mustLoad(t, requiredYAML, "server: {logLevel: debug}\nrateLimit: {reserveFraction: 0.25}\nupstream: {concurrency: 2}\n")

	if cfg.LogLevel != slog.LevelDebug || cfg.Upstream.ReserveFraction != 0.25 || cfg.Upstream.Concurrency != 2 {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadValidatesArchiveLimits(t *testing.T) {
	_, err := config.LoadYAML([]byte(requiredYAML + "archive: {maxBytes: 0, cacheMaxBytes: -1, timeout: 0s}\n"))
	if err == nil {
		t.Fatal("invalid archive limits accepted")
	}
	for _, want := range []string{"archive.maxBytes: must be greater", "archive.cacheMaxBytes: must be greater", "archive.timeout: must be a positive duration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}

	_, err = config.LoadYAML([]byte(requiredYAML + "archive: {maxBytes: 100, cacheMaxBytes: 99}\n"))
	if err == nil || !strings.Contains(err.Error(), "archive.cacheMaxBytes: must not be smaller than archive.maxBytes") {
		t.Errorf("cache smaller than one archive: %v", err)
	}

	cfg := mustLoad(t, requiredYAML, "archive: {maxBytes: 100, cacheMaxBytes: 100, timeout: 1m}\n")
	if cfg.Archive != (config.Archive{MaxBytes: 100, CacheMaxBytes: 100, Timeout: time.Minute}) {
		t.Errorf("archive = %+v", cfg.Archive)
	}
}
