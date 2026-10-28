package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// executor sends an already selected request under the quota and retry budget.
// It does not look up, refresh or write cached responses.
type executor struct {
	http         *http.Client
	limiter      *QuotaTracker
	retrier      *Retrier
	now          func() time.Time
	maxBodyBytes int64
}

func (e *executor) do(ctx context.Context, cl *call, etag string) (*Response, error) {
	var resp *Response
	err := e.retrier.Do(ctx, func(ctx context.Context) Attempt {
		var outcome Attempt
		resp, outcome = e.attempt(ctx, cl, etag)
		return outcome
	})
	return resp, err
}

func (e *executor) attempt(ctx context.Context, cl *call, etag string) (*Response, Attempt) {
	if err := e.limiter.Allow(cl.limitKey, cl.cost); err != nil {
		return nil, Attempt{Err: err}
	}
	httpResp, err := e.send(ctx, cl, etag)
	if err != nil {
		return nil, Attempt{Retry: ClassifyError(err), Err: err}
	}
	defer func() { _ = httpResp.Body.Close() }()
	body, err := readBody(httpResp, e.maxBodyBytes)
	if err != nil {
		return nil, Attempt{Retry: !errors.Is(err, errBodyTooLarge) && ClassifyError(err), Err: err}
	}
	e.limiter.UpdateFromHeaders(cl.limitKey, httpResp.Header)
	return e.handleStatus(cl, httpResp, body)
}

func (e *executor) handleStatus(cl *call, httpResp *http.Response, body []byte) (*Response, Attempt) {
	now := e.now()
	status := httpResp.StatusCode
	switch {
	case status == http.StatusUnauthorized:
		return nil, Attempt{Err: &StatusError{StatusCode: status}}
	case status >= 200 && status < 300:
		resp := &Response{StatusCode: status, Header: httpResp.Header, Body: body, FetchedAt: now}
		if cl.graphQL && !hasGraphQLErrors(body) {
			e.trackGraphQL(cl.limitKey, body)
		}
		return resp, Attempt{}
	}
	if retryable, after := ClassifyStatus(status, httpResp.Header, now); retryable {
		return nil, Attempt{Retry: true, After: after, Err: &StatusError{StatusCode: status, RetryAfter: after}}
	}
	return &Response{StatusCode: status, Header: httpResp.Header, Body: body, FetchedAt: now}, Attempt{}
}

func (e *executor) send(ctx context.Context, cl *call, etag string) (*http.Response, error) {
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
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return e.http.Do(req)
}

func (e *executor) trackGraphQL(key QuotaKey, body []byte) {
	var payload struct {
		Data struct {
			RateLimit *GraphQLInfo `json:"rateLimit"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Data.RateLimit != nil {
		e.limiter.UpdateFromGraphQL(key, *payload.Data.RateLimit)
	}
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
