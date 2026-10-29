package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/cache"
	"pdd/services/pkg/gitprovider/ratelimit"
	"pdd/services/pkg/gitprovider/retry"
)

// StreamResponse is a provider response whose body has not been read yet.
type StreamResponse struct {
	StatusCode int
	Header     http.Header
	// ContentLength is the length announced by the provider, -1 when unknown.
	ContentLength int64
	// Body is the live response body, set only for a 2xx status. The caller must close it.
	Body io.ReadCloser
}

// Stream sends a GET whose body is too large to buffer, such as a repository archive. It follows
// the request policy of Get (quota check, rate limit headers, retries, concurrency cap of the
// transport) but never caches or deduplicates, and does not limit the body size: the caller
// reads the body and enforces its own limit.
//
// Redirects are followed by the HTTP client, which drops the Authorization header when the
// target host differs. A 2xx answer is returned with its open body. A 401 returns an error
// matching ErrUnauthorized. Other final statuses are returned without a body so the caller can
// classify them.
func (c *Client) Stream(ctx context.Context, req Request) (*StreamResponse, error) {
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("client: invalid URL %q", req.URL)
	}
	cl := &call{
		method:   http.MethodGet,
		url:      req.URL,
		header:   req.Header,
		token:    req.Token,
		limitKey: ratelimit.NewKey(u.Host, req.Token),
		cost:     req.Cost,
		stream:   true,
	}
	var resp *StreamResponse
	err = c.cfg.Retrier.Do(ctx, func(ctx context.Context) retry.Attempt {
		r, attempt := c.streamAttempt(ctx, cl)
		resp = r
		return attempt
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) streamAttempt(ctx context.Context, cl *call) (*StreamResponse, retry.Attempt) {
	if err := c.cfg.Limiter.Allow(cl.limitKey, cl.cost); err != nil {
		return nil, retry.Attempt{Err: err}
	}
	httpResp, err := c.send(ctx, cl, cache.Entry{}, false)
	if err != nil {
		return nil, retry.Attempt{Retry: retry.ClassifyError(err), Err: err}
	}
	c.cfg.Limiter.UpdateFromHeaders(cl.limitKey, httpResp.Header)
	status := httpResp.StatusCode
	if status >= 200 && status < 300 {
		return &StreamResponse{
			StatusCode: status, Header: httpResp.Header, ContentLength: httpResp.ContentLength, Body: httpResp.Body,
		}, retry.Attempt{}
	}
	_ = httpResp.Body.Close()
	if status == http.StatusUnauthorized {
		return nil, retry.Attempt{Err: &gitprovider.StatusError{StatusCode: status}}
	}
	if retryable, after := retry.ClassifyStatus(status, httpResp.Header, c.cfg.Now()); retryable {
		return nil, retry.Attempt{Retry: true, After: after, Err: &gitprovider.StatusError{StatusCode: status, RetryAfter: after}}
	}
	return &StreamResponse{StatusCode: status, Header: httpResp.Header, ContentLength: -1}, retry.Attempt{}
}
