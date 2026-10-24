package retry_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/retry"
)

var epoch = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

type sleeper struct{ slept []time.Duration }

func (s *sleeper) sleep(_ context.Context, d time.Duration) error {
	s.slept = append(s.slept, d)
	return nil
}

func newRetrier(s *sleeper, random float64) *retry.Retrier {
	return &retry.Retrier{Policy: retry.DefaultPolicy(), Sleep: s.sleep, Rand: func() float64 { return random }}
}

func TestBackoffIsExponentialWithFullJitter(t *testing.T) {
	p := retry.Policy{MaxAttempts: 10, BaseDelay: time.Second, MaxDelay: 5 * time.Second}
	tests := []struct {
		attempt int
		random  float64
		want    time.Duration
	}{
		{1, 0.5, 500 * time.Millisecond},
		{2, 0.5, time.Second},
		{3, 0.5, 2 * time.Second},
		{4, 1, 5 * time.Second},
		{9, 0.999, 4995 * time.Millisecond},
		{3, 0, 0},
	}
	for _, tt := range tests {
		r := &retry.Retrier{Policy: p, Rand: func() float64 { return tt.random }}
		if got := r.Backoff(tt.attempt); got != tt.want {
			t.Errorf("Backoff(%d, rand %v) = %v, want %v", tt.attempt, tt.random, got, tt.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"seconds", "120", 120 * time.Second, true},
		{"seconds with spaces", " 7 ", 7 * time.Second, true},
		{"http date", epoch.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{"date in the past", epoch.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"empty", "", 0, false},
		{"negative", "-5", 0, false},
		{"garbage", "later", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := retry.ParseRetryAfter(tt.value, epoch)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ParseRetryAfter(%q) = %v, %v; want %v, %v", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestClassifyStatus(t *testing.T) {
	reset := strconv.FormatInt(epoch.Add(45*time.Second).Unix(), 10)
	tests := []struct {
		name      string
		status    int
		header    http.Header
		wantRetry bool
		wantAfter time.Duration
	}{
		{"500", 500, nil, true, 0},
		{"502", 502, nil, true, 0},
		{"503 with Retry-After", 503, http.Header{"Retry-After": {"4"}}, true, 4 * time.Second},
		{"429 plain", 429, nil, true, 0},
		{"429 with Retry-After", 429, http.Header{"Retry-After": {"10"}}, true, 10 * time.Second},
		{"429 quota exhausted", 429, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, true, 45 * time.Second},
		{"403 secondary limit by Retry-After", 403, http.Header{"Retry-After": {"60"}}, true, time.Minute},
		{"403 primary exhausted", 403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, true, 45 * time.Second},
		{"403 exhausted without reset", 403, http.Header{"X-Ratelimit-Remaining": {"0"}}, true, 0},
		{"Retry-After wins over reset", 403, http.Header{"Retry-After": {"3"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, true, 3 * time.Second},
		{"403 plain permission error", 403, http.Header{"X-Ratelimit-Remaining": {"4000"}}, false, 0},
		{"400", 400, nil, false, 0},
		{"401", 401, nil, false, 0},
		{"404", 404, nil, false, 0},
		{"422", 422, nil, false, 0},
		{"200", 200, nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.header
			if h == nil {
				h = http.Header{}
			}
			gotRetry, gotAfter := retry.ClassifyStatus(tt.status, h, epoch)
			if gotRetry != tt.wantRetry || gotAfter != tt.wantAfter {
				t.Errorf("got %v, %v; want %v, %v", gotRetry, gotAfter, tt.wantRetry, tt.wantAfter)
			}
		})
	}
}

func TestClassifyError(t *testing.T) {
	if !retry.ClassifyError(errors.New("connection reset")) {
		t.Error("network errors must be retried")
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded} {
		if retry.ClassifyError(err) {
			t.Errorf("%v must not be retried", err)
		}
	}
}

func TestDoSucceedsWithoutSleeping(t *testing.T) {
	s := &sleeper{}
	calls := 0
	err := newRetrier(s, 0.5).Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{}
	})
	if err != nil || calls != 1 || len(s.slept) != 0 {
		t.Fatalf("err=%v calls=%d slept=%v", err, calls, s.slept)
	}
}

func TestDoRetriesWithBackoffThenSucceeds(t *testing.T) {
	s := &sleeper{}
	calls := 0
	err := newRetrier(s, 0.5).Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		if calls < 3 {
			return retry.Attempt{Retry: true, Err: errors.New("boom")}
		}
		return retry.Attempt{}
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond}
	if len(s.slept) != 2 || s.slept[0] != want[0] || s.slept[1] != want[1] {
		t.Errorf("slept %v, want %v", s.slept, want)
	}
}

func TestDoMaxRetryReached(t *testing.T) {
	s := &sleeper{}
	cause := errors.New("boom")
	calls := 0
	err := newRetrier(s, 0.5).Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{Retry: true, Err: cause}
	})
	var mre *gitprovider.MaxRetryError
	if !errors.Is(err, gitprovider.ErrMaxRetry) || !errors.As(err, &mre) || mre.Attempts != 3 {
		t.Fatalf("want MaxRetryError with 3 attempts, got %v", err)
	}
	if !errors.Is(err, cause) {
		t.Error("last cause must stay reachable")
	}
	if calls != 3 || len(s.slept) != 2 {
		t.Errorf("calls=%d sleeps=%d, want 3 and 2", calls, len(s.slept))
	}
}

func TestDoHonorsRetryAfter(t *testing.T) {
	s := &sleeper{}
	calls := 0
	_ = newRetrier(s, 0.99).Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		if calls == 1 {
			return retry.Attempt{Retry: true, After: 12 * time.Second, Err: errors.New("limited")}
		}
		return retry.Attempt{}
	})
	if len(s.slept) != 1 || s.slept[0] != 12*time.Second {
		t.Errorf("slept %v, want [12s]", s.slept)
	}
}

func TestDoGivesUpWhenRetryAfterIsTooLong(t *testing.T) {
	s := &sleeper{}
	calls := 0
	err := newRetrier(s, 0.5).Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{Retry: true, After: time.Hour, Err: errors.New("limited")}
	})
	if !errors.Is(err, gitprovider.ErrMaxRetry) || calls != 1 || len(s.slept) != 0 {
		t.Fatalf("err=%v calls=%d slept=%v", err, calls, s.slept)
	}
}

func TestDoDoesNotRetryFinalErrors(t *testing.T) {
	s := &sleeper{}
	final := errors.New("bad request")
	calls := 0
	err := newRetrier(s, 0.5).Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{Err: final}
	})
	if !errors.Is(err, final) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDoStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := newRetrier(&sleeper{}, 0.5).Do(ctx, func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{}
	})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDoStopsWhenContextEndsDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &retry.Retrier{Policy: retry.DefaultPolicy(), Sleep: func(ctx context.Context, _ time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	calls := 0
	err := r.Do(ctx, func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{Retry: true, Err: errors.New("boom")}
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDefaultSleepAndRandom(t *testing.T) {
	r := &retry.Retrier{Policy: retry.Policy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxRetryAfter: time.Second}}
	calls := 0
	err := r.Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{Retry: calls == 1}
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slow := &retry.Retrier{Policy: retry.Policy{MaxAttempts: 2, BaseDelay: time.Hour, MaxDelay: time.Hour}}
	if err := slow.Do(ctx, func(context.Context) retry.Attempt { return retry.Attempt{Retry: true} }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestDoWithZeroMaxAttemptsTriesOnce(t *testing.T) {
	calls := 0
	r := &retry.Retrier{}
	err := r.Do(context.Background(), func(context.Context) retry.Attempt {
		calls++
		return retry.Attempt{Retry: true, Err: errors.New("x")}
	})
	if !errors.Is(err, gitprovider.ErrMaxRetry) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
