package gitgateway

import (
	"net/http"

	"pdd/services/internal/gitgateway/config"
	"pdd/services/internal/gitgateway/upstream"
)

// newUpstreamClient creates the shared provider client from the configuration. Every
// request it sends counts against the per-token concurrency cap.
func newUpstreamClient(cfg config.Config, store upstream.ResponseCache, onCacheError func(error)) (*upstream.Client, error) {
	up := cfg.Upstream
	return upstream.New(
		upstream.Config{
			HTTPClient: &http.Client{
				Timeout:   up.Timeout,
				Transport: upstream.NewLimitedTransport(http.DefaultTransport, up.Concurrency),
			},
			Cache: store,
			Limiter: upstream.NewQuotaTracker(upstream.QuotaConfig{
				ReserveFraction: up.ReserveFraction,
				ReserveMin:      up.ReserveMin,
			}),
			Retrier: &upstream.Retrier{Policy: upstream.Policy{
				MaxAttempts:   up.RetryMaxAttempts,
				BaseDelay:     up.RetryBaseDelay,
				MaxDelay:      up.RetryMaxDelay,
				MaxRetryAfter: up.RetryMaxRetryWait,
			}},
			DefaultTTL:   cfg.Cache.TTLPullList,
			StaleTTL:     cfg.Cache.StaleTTL,
			MaxBodyBytes: up.MaxBodyBytes,
			OnCacheError: onCacheError,
		})
}
