package upstream_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/config"
	"pdd/services/internal/gitgateway/upstream"
	"pdd/services/pkg/gitprovider/cache"
)

func TestLimitedTransportCapsConcurrencyPerToken(t *testing.T) {
	var active, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := active.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		<-release
		active.Add(-1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	client := &http.Client{Transport: upstream.NewLimitedTransport(http.DefaultTransport, 2)}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Authorization", "Bearer same-token")
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}
	waitFor(t, func() bool { return active.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := active.Load(); got != 2 {
		t.Errorf("active requests = %d, want the cap 2", got)
	}
	close(release)
	wg.Wait()
	if peak.Load() > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", peak.Load())
	}
}

func TestLimitedTransportDoesNotShareSlotsBetweenTokens(t *testing.T) {
	var active atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		active.Add(1)
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	client := &http.Client{Transport: upstream.NewLimitedTransport(http.DefaultTransport, 1)}

	var wg sync.WaitGroup
	for _, token := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if resp, err := client.Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	waitFor(t, func() bool { return active.Load() == 3 })
	close(release)
	wg.Wait()
}

func TestLimitedTransportHonorsContextWhileWaiting(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		started.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)
	client := &http.Client{Transport: upstream.NewLimitedTransport(http.DefaultTransport, 1)}
	go func() {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	waitFor(t, func() bool { return started.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("waiting request did not give up when its context expired")
	}
}

func TestLimitedTransportReleasesSlotOnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	client := &http.Client{Transport: upstream.NewLimitedTransport(http.DefaultTransport, 1)}

	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("request to a closed server succeeded")
		}
	}
}

func TestNewClientBuildsPolicyClient(t *testing.T) {
	cfg := config.Config{
		Upstream: config.Upstream{Concurrency: 4, RetryMaxAttempts: 3, Timeout: time.Second, MaxBodyBytes: 1 << 20, ReserveMin: 10, ReserveFraction: 0.1},
		Cache:    config.Cache{TTLPullList: time.Minute, StaleTTL: time.Hour},
	}
	c, err := upstream.NewClient(cfg, cache.NewMemoryStore(0, nil), nil)
	if err != nil || c == nil {
		t.Fatalf("NewClient = %v, %v", c, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
