package gitgateway

import (
	"testing"
	"time"

	"pdd/services/internal/gitgateway/config"
	"pdd/services/internal/gitgateway/upstream"
)

func TestNewClientBuildsPolicyClient(t *testing.T) {
	cfg := config.Config{
		Upstream: config.Upstream{Concurrency: 4, RetryMaxAttempts: 3, Timeout: time.Second, MaxBodyBytes: 1 << 20, ReserveMin: 10, ReserveFraction: 0.1},
		Cache:    config.Cache{TTLPullList: time.Minute, StaleTTL: time.Hour},
	}
	c, err := newUpstreamClient(cfg, upstream.NewMemoryStore(0, nil), nil)
	if err != nil || c == nil {
		t.Fatalf("NewClient = %v, %v", c, err)
	}
}
