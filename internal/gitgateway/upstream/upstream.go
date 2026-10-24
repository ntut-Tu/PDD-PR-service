// Package upstream builds the policy client used for every Git provider call.
package upstream

import (
	"crypto/sha256"
	"io"
	"net/http"
	"sync"

	"pdd/services/internal/gitgateway/config"
	"pdd/services/pkg/gitprovider/cache"
	"pdd/services/pkg/gitprovider/client"
	"pdd/services/pkg/gitprovider/ratelimit"
	"pdd/services/pkg/gitprovider/retry"
)

// NewClient creates the shared provider client from the configuration. Every
// request it sends counts against the per-token concurrency cap.
func NewClient(cfg config.Config, store cache.Store, onCacheError func(error)) (*client.Client, error) {
	up := cfg.Upstream
	return client.New(client.Config{
		HTTPClient: &http.Client{
			Timeout:   up.Timeout,
			Transport: NewLimitedTransport(http.DefaultTransport, up.Concurrency),
		},
		Cache: store,
		Limiter: ratelimit.NewTracker(ratelimit.Config{
			ReserveFraction: up.ReserveFraction,
			ReserveMin:      up.ReserveMin,
		}),
		Retrier: &retry.Retrier{Policy: retry.Policy{
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

// LimitedTransport caps the simultaneous requests per Authorization header, so
// one user's token never exceeds the provider's concurrency limit.
type LimitedTransport struct {
	next  http.RoundTripper
	limit int

	mu      sync.Mutex
	pending map[[sha256.Size]byte]*slot
}

type slot struct {
	sem  chan struct{}
	refs int
}

// NewLimitedTransport wraps next and allows at most limit concurrent requests per token.
func NewLimitedTransport(next http.RoundTripper, limit int) *LimitedTransport {
	return &LimitedTransport{next: next, limit: limit, pending: make(map[[sha256.Size]byte]*slot)}
}

// RoundTrip implements http.RoundTripper. The slot is held until the response
// body is closed, so a slow body read still counts as an active request.
func (t *LimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	s := t.acquireSlot(key)
	release := func() {
		<-s.sem
		t.releaseSlot(key, s)
	}
	select {
	case s.sem <- struct{}{}:
	case <-req.Context().Done():
		t.releaseSlot(key, s)
		return nil, req.Context().Err()
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: sync.OnceFunc(release)}
	return resp, nil
}

// releasingBody frees the concurrency slot when the body is closed.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func (t *LimitedTransport) acquireSlot(key [sha256.Size]byte) *slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.pending[key]
	if !ok {
		s = &slot{sem: make(chan struct{}, t.limit)}
		t.pending[key] = s
	}
	s.refs++
	return s
}

func (t *LimitedTransport) releaseSlot(key [sha256.Size]byte, s *slot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.refs--; s.refs == 0 {
		delete(t.pending, key)
	}
}
