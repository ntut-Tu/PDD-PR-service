package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/cache"
	"pdd/services/pkg/gitprovider/client"
	"pdd/services/pkg/gitprovider/ratelimit"
	"pdd/services/pkg/gitprovider/retry"
)

var epoch = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// fakeClock is a manual clock whose Sleep advances time instead of waiting.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	return nil
}

func (c *fakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

type fixture struct {
	clock   *fakeClock
	limiter *ratelimit.Tracker
	store   *cache.MemoryStore
	client  *client.Client
}

func newFixture(t *testing.T, mutate ...func(*client.Config)) *fixture {
	t.Helper()
	clock := &fakeClock{now: epoch}
	rlCfg := ratelimit.DefaultConfig()
	rlCfg.Now = clock.Now
	limiter := ratelimit.NewTracker(rlCfg)
	store := cache.NewMemoryStore(100, clock.Now)
	cfg := client.Config{
		Cache:      store,
		Limiter:    limiter,
		Retrier:    &retry.Retrier{Policy: retry.DefaultPolicy(), Sleep: clock.Sleep, Rand: func() float64 { return 0.5 }},
		DefaultTTL: time.Minute,
		StaleTTL:   time.Hour,
		Now:        clock.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{clock: clock, limiter: limiter, store: store, client: c}
}

func serve(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func quotaHeaders(w http.ResponseWriter, limit, remaining int, reset time.Time) {
	w.Header().Set("X-Ratelimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-Ratelimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(reset.Unix(), 10))
}

func get(t *testing.T, f *fixture, url string) (*client.Response, error) {
	t.Helper()
	return f.client.Get(context.Background(), client.Request{URL: url, Token: "secret-token"})
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := client.New(client.Config{}); err == nil {
		t.Fatal("expected an error for missing dependencies")
	}
}

func TestGetCachesFreshResponse(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `[1]`)
	})
	f := newFixture(t)
	first, err := get(t, f, srv.URL+"/pulls")
	if err != nil || first.FromCache || string(first.Body) != `[1]` {
		t.Fatalf("first: %+v err=%v", first, err)
	}
	second, err := get(t, f, srv.URL+"/pulls")
	if err != nil || !second.FromCache || second.Stale || string(second.Body) != `[1]` {
		t.Fatalf("second: %+v err=%v", second, err)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits = %d, want 1", hits.Load())
	}
}

func TestGetNotModifiedServesCachedBody(t *testing.T) {
	var sawConditional atomic.Bool
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			sawConditional.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `{"n":1}`)
	})
	f := newFixture(t)
	if _, err := get(t, f, srv.URL+"/pull/1"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Minute)

	resp, err := get(t, f, srv.URL+"/pull/1")
	if err != nil {
		t.Fatal(err)
	}
	if !sawConditional.Load() || hits.Load() != 2 {
		t.Fatalf("conditional=%v hits=%d", sawConditional.Load(), hits.Load())
	}
	if !resp.FromCache || resp.Stale || string(resp.Body) != `{"n":1}` || !resp.FetchedAt.Equal(f.clock.Now()) {
		t.Fatalf("unexpected response %+v", resp)
	}
	if _, err := get(t, f, srv.URL+"/pull/1"); err != nil || hits.Load() != 2 {
		t.Errorf("freshness must be renewed by the 304: err=%v hits=%d", err, hits.Load())
	}
}

func TestGetForceRefreshRevalidates(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `x`)
	})
	f := newFixture(t)
	_, _ = get(t, f, srv.URL)
	resp, err := f.client.Get(context.Background(), client.Request{URL: srv.URL, Token: "secret-token", ForceRefresh: true})
	if err != nil || !resp.FromCache || hits.Load() != 2 {
		t.Fatalf("resp=%+v err=%v hits=%d", resp, err, hits.Load())
	}
}

func TestGetQuotaReserveWithoutCache(t *testing.T) {
	reset := epoch.Add(30 * time.Minute)
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		quotaHeaders(w, 5000, 120, reset)
		_, _ = io.WriteString(w, `ok`)
	})
	f := newFixture(t)
	if _, err := get(t, f, srv.URL+"/a"); err != nil {
		t.Fatal(err)
	}
	_, err := get(t, f, srv.URL+"/b")
	var qe *gitprovider.QuotaReserveError
	if !errors.Is(err, gitprovider.ErrQuotaReserve) || !errors.As(err, &qe) || !qe.ResetAt.Equal(reset) {
		t.Fatalf("want QuotaReserveError resetting at %v, got %v", reset, err)
	}
	if hits.Load() != 1 {
		t.Errorf("no request may be sent while in reserve, hits=%d", hits.Load())
	}
}

func TestGetQuotaReserveServesStaleCache(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		quotaHeaders(w, 5000, 100, epoch.Add(time.Hour))
		_, _ = io.WriteString(w, `cached`)
	})
	f := newFixture(t)
	_, _ = get(t, f, srv.URL)
	f.clock.Advance(5 * time.Minute)

	resp, err := get(t, f, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Stale || !resp.FromCache || string(resp.Body) != `cached` || !errors.Is(resp.StaleReason, gitprovider.ErrQuotaReserve) {
		t.Fatalf("unexpected response %+v", resp)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
}

func TestGetQuotaRecoversAfterReset(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		quotaHeaders(w, 5000, 0, epoch.Add(10*time.Minute))
		_, _ = io.WriteString(w, `ok`)
	})
	f := newFixture(t)
	_, _ = get(t, f, srv.URL)
	f.clock.Advance(11 * time.Minute)
	if _, err := get(t, f, srv.URL); err != nil || hits.Load() != 2 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestGetRetriesSecondaryRateLimitWithRetryAfterSeconds(t *testing.T) {
	var calls atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `ok`)
	})
	f := newFixture(t)
	resp, err := get(t, f, srv.URL)
	if err != nil || string(resp.Body) != `ok` {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if got := f.clock.Slept(); len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("slept %v, want [7s]", got)
	}
}

func TestGetRetriesWithRetryAfterHTTPDate(t *testing.T) {
	var calls atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", epoch.Add(15*time.Second).Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `ok`)
	})
	f := newFixture(t)
	if _, err := get(t, f, srv.URL); err != nil {
		t.Fatal(err)
	}
	if got := f.clock.Slept(); len(got) != 1 || got[0] != 15*time.Second {
		t.Errorf("slept %v, want [15s]", got)
	}
}

func TestGetPrimaryLimitExhaustedStopsRetrying(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		quotaHeaders(w, 5000, 0, epoch.Add(40*time.Minute))
		w.WriteHeader(http.StatusForbidden)
	})
	f := newFixture(t)
	_, err := get(t, f, srv.URL)
	if !errors.Is(err, gitprovider.ErrMaxRetry) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	if _, err := get(t, f, srv.URL); !errors.Is(err, gitprovider.ErrQuotaReserve) || hits.Load() != 1 {
		t.Fatalf("the exhausted quota must block later requests: err=%v hits=%d", err, hits.Load())
	}
}

func TestGetMaxRetryReached(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	f := newFixture(t)
	_, err := get(t, f, srv.URL)
	var status *gitprovider.StatusError
	if !errors.Is(err, gitprovider.ErrMaxRetry) || !errors.As(err, &status) || status.StatusCode != 502 {
		t.Fatalf("want ErrMaxRetry wrapping 502, got %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
	if got := f.clock.Slept(); len(got) != 2 || got[0] != 250*time.Millisecond || got[1] != 500*time.Millisecond {
		t.Errorf("slept %v, want [250ms 500ms]", got)
	}
}

func TestGetMaxRetryServesStaleCache(t *testing.T) {
	var fail atomic.Bool
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `old`)
	})
	f := newFixture(t)
	_, _ = get(t, f, srv.URL)
	fail.Store(true)
	f.clock.Advance(5 * time.Minute)

	resp, err := get(t, f, srv.URL)
	if err != nil || !resp.Stale || string(resp.Body) != `old` || !errors.Is(resp.StaleReason, gitprovider.ErrMaxRetry) {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestGetRetriesNetworkError(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `ok`) })
	var attempts atomic.Int32
	f := newFixture(t, func(c *client.Config) {
		c.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, errors.New("connection reset by peer")
			}
			return http.DefaultTransport.RoundTrip(r)
		})}
	})
	resp, err := get(t, f, srv.URL)
	if err != nil || string(resp.Body) != `ok` || attempts.Load() != 2 {
		t.Fatalf("resp=%+v err=%v attempts=%d", resp, err, attempts.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGetDoesNotRetryOtherClientErrors(t *testing.T) {
	for _, status := range []int{400, 403, 404, 422} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `nope`)
			})
			f := newFixture(t)
			resp, err := get(t, f, srv.URL)
			if err != nil || resp.StatusCode != status || string(resp.Body) != `nope` {
				t.Fatalf("resp=%+v err=%v", resp, err)
			}
			if hits.Load() != 1 || len(f.clock.Slept()) != 0 {
				t.Errorf("hits=%d slept=%v", hits.Load(), f.clock.Slept())
			}
			if again, _ := get(t, f, srv.URL); hits.Load() != 2 || again.FromCache {
				t.Error("error responses must not be cached")
			}
		})
	}
}

func TestGetUnauthorized(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	f := newFixture(t)
	_, err := get(t, f, srv.URL)
	if !errors.Is(err, gitprovider.ErrUnauthorized) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestGetContextCancelledDuringBackoff(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	ctx, cancel := context.WithCancel(context.Background())
	f := newFixture(t, func(c *client.Config) {
		c.Retrier.Sleep = func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}
	})
	_, err := f.client.Get(ctx, client.Request{URL: srv.URL, Token: "t"})
	if !errors.Is(err, context.Canceled) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestGetContextCancelledWhileWaiting(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	srv, _ := serve(t, func(_ http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
	})
	defer close(release)
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := f.client.Get(ctx, client.Request{URL: srv.URL, Token: "t"})
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestGetDeduplicatesConcurrentRequests(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 20)
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `shared`)
	})
	f := newFixture(t)
	const callers = 10
	var wg sync.WaitGroup
	bodies := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := get(t, f, srv.URL)
			if err != nil {
				t.Error(err)
				return
			}
			bodies[i] = string(resp.Body)
		}(i)
	}
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits.Load() != 1 {
		t.Errorf("server hits = %d, want 1", hits.Load())
	}
	for i, b := range bodies {
		if b != "shared" {
			t.Errorf("caller %d got %q", i, b)
		}
	}
}

func TestGetDifferentTokensAreNotShared(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	})
	f := newFixture(t)
	a, _ := f.client.Get(context.Background(), client.Request{URL: srv.URL, Token: "alice"})
	b, _ := f.client.Get(context.Background(), client.Request{URL: srv.URL, Token: "bob"})
	if string(a.Body) == string(b.Body) || hits.Load() != 2 {
		t.Fatalf("tokens must not share cache: %q %q hits=%d", a.Body, b.Body, hits.Load())
	}
}

func TestGetSharedFlightSurvivesLeaderCancel(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-release:
			_, _ = io.WriteString(w, `done`)
		case <-r.Context().Done():
		}
	})
	f := newFixture(t)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := f.client.Get(leaderCtx, client.Request{URL: srv.URL, Token: "t"})
		leaderErr <- err
	}()
	<-started
	followerRes := make(chan string, 1)
	go func() {
		resp, err := get(t, f, srv.URL)
		if err != nil {
			followerRes <- err.Error()
			return
		}
		followerRes <- string(resp.Body)
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err=%v", err)
	}
	<-started
	close(release)
	if got := <-followerRes; got != "done" {
		t.Fatalf("follower got %q", got)
	}
}

func TestGetRejectsInvalidURL(t *testing.T) {
	f := newFixture(t)
	if _, err := f.client.Get(context.Background(), client.Request{URL: "::bad"}); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := f.client.GraphQL(context.Background(), client.GraphQLRequest{Endpoint: "/graphql"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGetRejectsOversizedBody(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, strings.Repeat("x", 100)) })
	f := newFixture(t, func(c *client.Config) { c.MaxBodyBytes = 10 })
	if _, err := get(t, f, srv.URL); err == nil || errors.Is(err, gitprovider.ErrMaxRetry) || hits.Load() != 1 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
}

func TestCacheErrorsDegradeToUncachedRequests(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `ok`) })
	var reported atomic.Int32
	f := newFixture(t, func(c *client.Config) {
		c.Cache = brokenStore{}
		c.OnCacheError = func(error) { reported.Add(1) }
	})
	for i := 0; i < 2; i++ {
		if resp, err := get(t, f, srv.URL); err != nil || string(resp.Body) != `ok` {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	}
	if hits.Load() != 2 || reported.Load() == 0 {
		t.Errorf("hits=%d reported=%d", hits.Load(), reported.Load())
	}
}

type brokenStore struct{}

func (brokenStore) Get(context.Context, string) (cache.Entry, bool, error) {
	return cache.Entry{}, false, errors.New("disk failure")
}
func (brokenStore) Set(context.Context, cache.Entry) error { return errors.New("disk failure") }
func (brokenStore) Delete(context.Context, string) error   { return nil }

func graphQLRequest(url string, vars map[string]any) client.GraphQLRequest {
	return client.GraphQLRequest{Endpoint: url, Token: "secret-token", Query: "query($n:Int!){rateLimit{cost remaining resetAt limit}}", Variables: vars}
}

func TestGraphQLCachesByQueryAndVariablesAndTracksRateLimit(t *testing.T) {
	reset := epoch.Add(time.Hour)
	srv, hits := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("If-None-Match") != "" {
			t.Errorf("method=%s If-None-Match=%q", r.Method, r.Header.Get("If-None-Match"))
		}
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Query, "rateLimit") {
			t.Errorf("bad body: %+v err=%v", body, err)
		}
		_, _ = io.WriteString(w, `{"data":{"rateLimit":{"cost":1,"remaining":50,"limit":5000,"resetAt":"`+reset.Format(time.RFC3339)+`"}}}`)
	})
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.client.GraphQL(ctx, graphQLRequest(srv.URL, map[string]any{"n": 1, "a": "x"})); err != nil {
		t.Fatal(err)
	}
	if q, ok := f.limiter.Quota(ratelimit.NewKey(strings.TrimPrefix(srv.URL, "http://"), "secret-token")); !ok || q.Remaining != 50 {
		t.Fatalf("quota not tracked: %+v ok=%v", q, ok)
	}
	resp, err := f.client.GraphQL(ctx, graphQLRequest(srv.URL, map[string]any{"a": "x", "n": 1}))
	if err != nil || !resp.FromCache || hits.Load() != 1 {
		t.Fatalf("same variables must hit the cache: resp=%+v err=%v hits=%d", resp, err, hits.Load())
	}
	if _, err := f.client.GraphQL(ctx, graphQLRequest(srv.URL, map[string]any{"n": 2})); !errors.Is(err, gitprovider.ErrQuotaReserve) {
		t.Fatalf("remaining 50 of 5000 must be in reserve, got %v", err)
	}
}

func TestGraphQLResponsesWithErrorsAreNotCached(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":null,"errors":[{"type":"NOT_FOUND"}]}`)
	})
	f := newFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := f.client.GraphQL(context.Background(), graphQLRequest(srv.URL, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}

func TestGraphQLRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, `{"data":{}}`)
	})
	f := newFixture(t)
	if _, err := f.client.GraphQL(context.Background(), graphQLRequest(srv.URL, nil)); err != nil || calls.Load() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestGraphQLRejectsUnencodableVariables(t *testing.T) {
	f := newFixture(t)
	_, err := f.client.GraphQL(context.Background(), graphQLRequest("https://api.github.com/graphql", map[string]any{"bad": make(chan int)}))
	if err == nil {
		t.Fatal("expected an encoding error")
	}
}
