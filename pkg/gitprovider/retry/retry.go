// Package retry implements the retry policy for GitHub and GitLab requests:
// error classification, Retry-After parsing and bounded exponential backoff
// with full jitter.
package retry

import (
	"context"
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pdd/services/pkg/gitprovider"
)

// Policy configures the retry budget.
type Policy struct {
	// MaxAttempts is the total number of tries, including the first one.
	MaxAttempts int
	// BaseDelay is the backoff ceiling of the first retry; it doubles per retry.
	BaseDelay time.Duration
	// MaxDelay caps the backoff ceiling.
	MaxDelay time.Duration
	// MaxRetryAfter is the longest server-requested wait that is honored. A longer
	// request ends the retries at once, so callers can serve cached data instead.
	MaxRetryAfter time.Duration
}

// DefaultPolicy returns the defaults documented in the ADR.
func DefaultPolicy() Policy {
	return Policy{
		MaxAttempts:   3,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      8 * time.Second,
		MaxRetryAfter: 30 * time.Second,
	}
}

// Attempt is the outcome of one try.
type Attempt struct {
	// Retry asks for another try when the budget allows it.
	Retry bool
	// After is the wait requested by the server, zero when none.
	After time.Duration
	// Err is the failure of this try; nil on success.
	Err error
}

// Retrier runs operations under a Policy. The zero value of the injectable
// fields selects the real clock, sleep and random source.
type Retrier struct {
	Policy Policy
	// Sleep waits for d or until ctx is done. Defaults to a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand returns a number in [0,1). Defaults to math/rand.
	Rand func() float64
}

// Do calls try until it succeeds, reports a non-retryable result, the budget is
// spent or ctx is done. When the budget is spent, or the server asks for a wait
// longer than MaxRetryAfter, it returns a *gitprovider.MaxRetryError wrapping the
// last cause. Otherwise it returns the Err of the last Attempt.
func (r *Retrier) Do(ctx context.Context, try func(ctx context.Context) Attempt) error {
	maxAttempts := max(r.Policy.MaxAttempts, 1)
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		res := try(ctx)
		if !res.Retry {
			return res.Err
		}
		if attempt >= maxAttempts || res.After > r.Policy.MaxRetryAfter {
			return &gitprovider.MaxRetryError{Attempts: attempt, Err: res.Err}
		}
		if err := r.sleep(ctx, r.wait(attempt, res.After)); err != nil {
			return err
		}
	}
}

// Backoff returns the full-jitter delay before retry number attempt (1-based):
// a random value in [0, min(MaxDelay, BaseDelay*2^(attempt-1))).
func (r *Retrier) Backoff(attempt int) time.Duration {
	ceiling := r.Policy.BaseDelay
	for i := 1; i < attempt && ceiling < r.Policy.MaxDelay; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, r.Policy.MaxDelay)
	return time.Duration(r.random() * float64(ceiling))
}

func (r *Retrier) wait(attempt int, after time.Duration) time.Duration {
	if after > 0 {
		return after
	}
	return r.Backoff(attempt)
}

func (r *Retrier) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Retrier) random() float64 {
	if r.Rand != nil {
		return r.Rand()
	}
	return rand.Float64()
}

// ClassifyError reports whether a transport error is worth retrying. Network
// errors are retried; cancellation of the caller's context is not.
func ClassifyError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// ClassifyStatus decides whether a response status is retryable and how long
// the server asks to wait.
//
//   - 5xx and 429 are retried.
//   - 403 is retried only as a GitHub secondary rate limit: the response carries
//     Retry-After or x-ratelimit-remaining: 0.
//   - Every other status, including all other 4xx, is final.
//
// The wait is Retry-After when present, else the time until x-ratelimit-reset
// when the quota is exhausted, else zero (the caller backs off).
func ClassifyStatus(status int, h http.Header, now time.Time) (retry bool, after time.Duration) {
	after, hasAfter := ParseRetryAfter(h.Get("Retry-After"), now)
	exhausted := strings.TrimSpace(h.Get("X-Ratelimit-Remaining")) == "0"
	if !hasAfter && exhausted {
		after = untilReset(h, now)
	}
	switch {
	case status >= 500, status == http.StatusTooManyRequests:
		return true, after
	case status == http.StatusForbidden && (hasAfter || exhausted):
		return true, after
	default:
		return false, 0
	}
}

// ParseRetryAfter parses a Retry-After value given as delay-seconds or as an
// HTTP date. Dates in the past yield a zero duration.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	ts, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(ts.Sub(now), 0), true
}

func untilReset(h http.Header, now time.Time) time.Duration {
	secs, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-Ratelimit-Reset")), 10, 64)
	if err != nil {
		return 0
	}
	return max(time.Unix(secs, 0).Sub(now), 0)
}
