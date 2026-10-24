// Package client sends GitHub and GitLab API requests under the shared request
// policy: quota check before sending, conditional GET with ETag, cached
// responses with fresh and stale lifetimes, deduplication of identical
// concurrent requests and bounded retries.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/sync/singleflight"

	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/cache"
	"pdd/services/pkg/gitprovider/ratelimit"
	"pdd/services/pkg/gitprovider/retry"
)

var errBodyTooLarge = errors.New("client: response body too large")

// Config wires the policy components together.
type Config struct {
	// HTTPClient sends the requests. Defaults to a client with a 30s timeout.
	HTTPClient *http.Client
	// Cache stores responses. Required.
	Cache cache.Store
	// Limiter tracks the quota. Required.
	Limiter *ratelimit.Tracker
	// Retrier runs the retry policy. Required.
	Retrier *retry.Retrier
	// DefaultTTL is the fresh lifetime of responses whose request sets none.
	DefaultTTL time.Duration
	// StaleTTL is how long an entry stays usable as stale data after it stopped
	// being fresh.
	StaleTTL time.Duration
	// MaxBodyBytes limits the size of a response body.
	MaxBodyBytes int64
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// OnCacheError is called when the cache fails. The request then proceeds as
	// if the cache missed. May be nil.
	OnCacheError func(err error)
}

// Client executes provider requests under the policy. It is safe for concurrent use.
type Client struct {
	cfg    Config
	flight singleflight.Group
}

// New validates cfg and creates a Client.
func New(cfg Config) (*Client, error) {
	if cfg.Cache == nil || cfg.Limiter == nil || cfg.Retrier == nil {
		return nil, errors.New("client: Cache, Limiter and Retrier are required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 10 << 20
	}
	return &Client{cfg: cfg}, nil
}

// Request describes a REST GET.
type Request struct {
	// URL is the absolute API URL.
	URL string
	// Token is the provider access token, sent as a Bearer token. It is hashed
	// for cache and quota keys and never stored.
	Token string
	// Header holds extra request headers such as Accept.
	Header http.Header
	// TTL is the fresh lifetime of the response; zero uses Config.DefaultTTL.
	TTL time.Duration
	// Cost is the quota cost of the request; zero means 1.
	Cost int
	// ForceRefresh skips the fresh cache check and revalidates with the provider.
	ForceRefresh bool
}

// GraphQLRequest describes a GraphQL query. Queries should select
// rateLimit { cost remaining resetAt limit } on GitHub so the tracker learns the real cost.
type GraphQLRequest struct {
	// Endpoint is the GraphQL URL, e.g. https://api.github.com/graphql.
	Endpoint string
	Token    string
	Query    string
	// Variables are encoded as JSON; map keys are sorted, so equal variables
	// always produce the same cache key.
	Variables map[string]any
	TTL       time.Duration
	// Cost is the expected quota cost; zero means 1.
	Cost         int
	ForceRefresh bool
}

// Response is a provider response, fresh or cached.
type Response struct {
	StatusCode int
	Header     http.Header
	// Body must be treated as read-only; it may be shared with other callers.
	Body []byte
	// FetchedAt is when the provider last confirmed the data.
	FetchedAt time.Time
	// FromCache is true when no request was needed or the provider answered 304.
	FromCache bool
	// Stale is true when the data is past its fresh lifetime because the
	// provider could not be asked. StaleReason then holds the cause: it matches
	// gitprovider.ErrQuotaReserve or gitprovider.ErrMaxRetry.
	Stale       bool
	StaleReason error
}

// Get performs a conditional, cached, deduplicated GET.
//
// Non-retryable 4xx answers other than 401 are returned as a Response without
// error and are not cached. A 401 returns an error matching ErrUnauthorized.
// When the quota is in reserve or the retries are spent and a cached entry
// exists, that entry is returned with Stale set; otherwise the error is
// returned and matches ErrQuotaReserve or ErrMaxRetry.
func (c *Client) Get(ctx context.Context, req Request) (*Response, error) {
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("client: invalid URL %q", req.URL)
	}
	key := ratelimit.NewKey(u.Host, req.Token)
	return c.execute(ctx, &call{
		method:       http.MethodGet,
		url:          req.URL,
		header:       req.Header,
		token:        req.Token,
		limitKey:     key,
		cacheKey:     cache.Key("rest", u.Host, key.TokenHash, req.URL),
		ttl:          req.TTL,
		cost:         req.Cost,
		conditional:  true,
		forceRefresh: req.ForceRefresh,
	})
}

// GraphQL performs a cached, deduplicated GraphQL POST. The cache key is the
// hash of endpoint, token fingerprint, query and variables; GraphQL has no
// conditional requests, so the cache only expires by TTL. Responses carrying a
// top-level "errors" member are returned but not cached.
func (c *Client) GraphQL(ctx context.Context, req GraphQLRequest) (*Response, error) {
	u, err := url.Parse(req.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("client: invalid URL %q", req.Endpoint)
	}
	vars, err := json.Marshal(req.Variables)
	if err != nil {
		return nil, fmt.Errorf("client: encode variables: %w", err)
	}
	body, err := json.Marshal(map[string]any{"query": req.Query, "variables": req.Variables})
	if err != nil {
		return nil, fmt.Errorf("client: encode query: %w", err)
	}
	key := ratelimit.NewKey(u.Host, req.Token)
	return c.execute(ctx, &call{
		method:       http.MethodPost,
		url:          req.Endpoint,
		header:       http.Header{"Content-Type": {"application/json"}},
		body:         body,
		token:        req.Token,
		limitKey:     key,
		cacheKey:     cache.Key("graphql", u.Host, key.TokenHash, req.Endpoint, req.Query, string(vars)),
		ttl:          req.TTL,
		cost:         req.Cost,
		graphQL:      true,
		forceRefresh: req.ForceRefresh,
	})
}

type call struct {
	method       string
	url          string
	header       http.Header
	body         []byte
	token        string
	limitKey     ratelimit.Key
	cacheKey     string
	ttl          time.Duration
	cost         int
	conditional  bool
	graphQL      bool
	forceRefresh bool
}

func (c *Client) execute(ctx context.Context, cl *call) (*Response, error) {
	entry, cached := c.lookup(ctx, cl.cacheKey)
	if cached && !cl.forceRefresh && entry.Fresh(c.cfg.Now()) {
		return entryResponse(entry, false, nil), nil
	}
	for {
		ch := c.flight.DoChan(cl.cacheKey, func() (any, error) { return c.fetch(ctx, cl, entry, cached) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-ch:
			// A shared flight led by another caller may have been cancelled by that
			// caller; this caller is still interested, so it starts over.
			if res.Err != nil && ctx.Err() == nil && errors.Is(res.Err, context.Canceled) {
				continue
			}
			if res.Err != nil {
				return nil, res.Err
			}
			return cloneResponse(res.Val.(*Response)), nil
		}
	}
}

func (c *Client) fetch(ctx context.Context, cl *call, entry cache.Entry, cached bool) (*Response, error) {
	var resp *Response
	err := c.cfg.Retrier.Do(ctx, func(ctx context.Context) retry.Attempt {
		r, attempt := c.attempt(ctx, cl, entry, cached)
		resp = r
		return attempt
	})
	if err == nil {
		return resp, nil
	}
	if cached && (errors.Is(err, gitprovider.ErrQuotaReserve) || errors.Is(err, gitprovider.ErrMaxRetry)) {
		return entryResponse(entry, true, err), nil
	}
	return nil, err
}

func (c *Client) attempt(ctx context.Context, cl *call, entry cache.Entry, cached bool) (*Response, retry.Attempt) {
	if err := c.cfg.Limiter.Allow(cl.limitKey, cl.cost); err != nil {
		return nil, retry.Attempt{Err: err}
	}
	httpResp, err := c.send(ctx, cl, entry, cached)
	if err != nil {
		return nil, retry.Attempt{Retry: retry.ClassifyError(err), Err: err}
	}
	defer func() { _ = httpResp.Body.Close() }()
	body, err := readBody(httpResp, c.cfg.MaxBodyBytes)
	if err != nil {
		return nil, retry.Attempt{Retry: !errors.Is(err, errBodyTooLarge) && retry.ClassifyError(err), Err: err}
	}
	c.cfg.Limiter.UpdateFromHeaders(cl.limitKey, httpResp.Header)
	return c.handleStatus(ctx, cl, entry, cached, httpResp, body)
}

func (c *Client) handleStatus(ctx context.Context, cl *call, entry cache.Entry, cached bool,
	httpResp *http.Response, body []byte) (*Response, retry.Attempt) {
	now := c.cfg.Now()
	status := httpResp.StatusCode
	switch {
	case status == http.StatusNotModified && cached:
		entry = c.refresh(ctx, cl, entry)
		return entryResponse(entry, false, nil), retry.Attempt{}
	case status == http.StatusUnauthorized:
		return nil, retry.Attempt{Err: &gitprovider.StatusError{StatusCode: status}}
	case status >= 200 && status < 300:
		resp := &Response{StatusCode: status, Header: httpResp.Header, Body: body, FetchedAt: now}
		c.store(ctx, cl, resp, now)
		return resp, retry.Attempt{}
	}
	if retryable, after := retry.ClassifyStatus(status, httpResp.Header, now); retryable {
		return nil, retry.Attempt{Retry: true, After: after, Err: &gitprovider.StatusError{StatusCode: status, RetryAfter: after}}
	}
	return &Response{StatusCode: status, Header: httpResp.Header, Body: body, FetchedAt: now}, retry.Attempt{}
}

func (c *Client) send(ctx context.Context, cl *call, entry cache.Entry, cached bool) (*http.Response, error) {
	var body io.Reader
	if cl.body != nil {
		body = bytes.NewReader(cl.body)
	}
	req, err := http.NewRequestWithContext(ctx, cl.method, cl.url, body)
	if err != nil {
		return nil, err
	}
	for name, values := range cl.header {
		req.Header[name] = append([]string(nil), values...)
	}
	if cl.token != "" {
		req.Header.Set("Authorization", "Bearer "+cl.token)
	}
	if cl.conditional && cached && entry.ETag != "" {
		req.Header.Set("If-None-Match", entry.ETag)
	}
	return c.cfg.HTTPClient.Do(req)
}

func (c *Client) lookup(ctx context.Context, key string) (cache.Entry, bool) {
	entry, ok, err := c.cfg.Cache.Get(ctx, key)
	if err != nil {
		c.cacheError(err)
		return cache.Entry{}, false
	}
	return entry, ok
}

// store caches a successful response unless it is a GraphQL answer with errors.
func (c *Client) store(ctx context.Context, cl *call, resp *Response, now time.Time) {
	if cl.graphQL {
		if hasGraphQLErrors(resp.Body) {
			return
		}
		c.trackGraphQL(cl.limitKey, resp.Body)
	}
	ttl := c.ttl(cl)
	err := c.cfg.Cache.Set(ctx, cache.Entry{
		Key:        cl.cacheKey,
		Value:      resp.Body,
		ETag:       resp.Header.Get("ETag"),
		FetchedAt:  now,
		FreshUntil: now.Add(ttl),
		EvictAfter: now.Add(ttl + c.cfg.StaleTTL),
	})
	if err != nil {
		c.cacheError(err)
	}
}

// refresh extends the freshness of an entry the provider confirmed with 304.
func (c *Client) refresh(ctx context.Context, cl *call, entry cache.Entry) cache.Entry {
	now := c.cfg.Now()
	ttl := c.ttl(cl)
	entry.FetchedAt = now
	entry.FreshUntil = now.Add(ttl)
	entry.EvictAfter = now.Add(ttl + c.cfg.StaleTTL)
	if err := c.cfg.Cache.Set(ctx, entry); err != nil {
		c.cacheError(err)
	}
	return entry
}

func (c *Client) ttl(cl *call) time.Duration {
	if cl.ttl > 0 {
		return cl.ttl
	}
	return c.cfg.DefaultTTL
}

func (c *Client) cacheError(err error) {
	if c.cfg.OnCacheError != nil {
		c.cfg.OnCacheError(err)
	}
}

func (c *Client) trackGraphQL(key ratelimit.Key, body []byte) {
	var payload struct {
		Data struct {
			RateLimit *ratelimit.GraphQLInfo `json:"rateLimit"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Data.RateLimit != nil {
		c.cfg.Limiter.UpdateFromGraphQL(key, *payload.Data.RateLimit)
	}
}

func hasGraphQLErrors(body []byte) bool {
	var payload struct {
		Errors json.RawMessage `json:"errors"`
	}
	return json.Unmarshal(body, &payload) == nil && len(payload.Errors) > 0 && string(payload.Errors) != "null"
}

func readBody(resp *http.Response, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", errBodyTooLarge, limit)
	}
	return body, nil
}

func entryResponse(e cache.Entry, stale bool, reason error) *Response {
	return &Response{
		StatusCode:  http.StatusOK,
		Header:      http.Header{"Etag": {e.ETag}},
		Body:        e.Value,
		FetchedAt:   e.FetchedAt,
		FromCache:   true,
		Stale:       stale,
		StaleReason: reason,
	}
}

func cloneResponse(r *Response) *Response {
	cp := *r
	cp.Header = r.Header.Clone()
	return &cp
}
