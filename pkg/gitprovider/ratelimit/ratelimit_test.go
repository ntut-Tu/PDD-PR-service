package ratelimit_test

import (
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/ratelimit"
)

var epoch = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func newTracker(now *time.Time) *ratelimit.Tracker {
	cfg := ratelimit.DefaultConfig()
	cfg.Now = func() time.Time { return *now }
	return ratelimit.NewTracker(cfg)
}

func TestNewKeyHashesToken(t *testing.T) {
	a := ratelimit.NewKey("API.GitHub.com", "ghp_secret")
	b := ratelimit.NewKey("api.github.com", "ghp_secret")
	c := ratelimit.NewKey("api.github.com", "other")
	if a != b {
		t.Errorf("host should be case-insensitive: %v vs %v", a, b)
	}
	if a == c {
		t.Error("different tokens must give different keys")
	}
	if a.TokenHash == "" || a.TokenHash == "ghp_secret" {
		t.Errorf("token must be hashed, got %q", a.TokenHash)
	}
}

func TestAllowUnknownKey(t *testing.T) {
	now := epoch
	if err := newTracker(&now).Allow(ratelimit.NewKey("h", "t"), 1); err != nil {
		t.Fatalf("unknown quota must be allowed: %v", err)
	}
}

func TestAllowRespectsReserve(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		remaining int
		cost      int
		wantErr   bool
	}{
		{"plenty left", 5000, 4000, 1, false},
		{"exactly at reserve after cost", 5000, 501, 1, false},
		{"one below reserve after cost", 5000, 500, 1, true},
		{"reserve min applies to small windows", 50, 10, 1, true},
		{"expensive query would cross reserve", 5000, 600, 200, true},
		{"zero cost counts as one", 5000, 500, 0, true},
		{"exhausted", 5000, 0, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := epoch
			tr := newTracker(&now)
			key := ratelimit.NewKey("api.github.com", "tok")
			tr.Set(key, ratelimit.Quota{Limit: tt.limit, Remaining: tt.remaining, ResetAt: now.Add(time.Hour)})
			err := tr.Allow(key, tt.cost)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Allow() = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, gitprovider.ErrQuotaReserve) {
				t.Errorf("error %v does not match ErrQuotaReserve", err)
			}
		})
	}
}

func TestAllowReportsResetTime(t *testing.T) {
	now := epoch
	tr := newTracker(&now)
	key := ratelimit.NewKey("api.github.com", "tok")
	reset := now.Add(20 * time.Minute)
	tr.Set(key, ratelimit.Quota{Limit: 5000, Remaining: 0, ResetAt: reset})
	var qe *gitprovider.QuotaReserveError
	if err := tr.Allow(key, 1); !errors.As(err, &qe) || !qe.ResetAt.Equal(reset) {
		t.Fatalf("want QuotaReserveError with reset %v, got %v", reset, err)
	}
}

func TestQuotaExpiresAtReset(t *testing.T) {
	now := epoch
	tr := newTracker(&now)
	key := ratelimit.NewKey("api.github.com", "tok")
	tr.Set(key, ratelimit.Quota{Limit: 5000, Remaining: 0, ResetAt: now.Add(time.Minute)})
	now = now.Add(time.Minute)
	if err := tr.Allow(key, 1); err != nil {
		t.Fatalf("window is over, request must be allowed: %v", err)
	}
	if _, ok := tr.Quota(key); ok {
		t.Error("expired window should be forgotten")
	}
}

func TestParseHeaders(t *testing.T) {
	reset := epoch.Add(10 * time.Minute)
	tests := []struct {
		name   string
		header http.Header
		want   ratelimit.Quota
		ok     bool
	}{
		{
			"github",
			http.Header{"X-Ratelimit-Limit": {"5000"}, "X-Ratelimit-Remaining": {"4990"}, "X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)}},
			ratelimit.Quota{Limit: 5000, Remaining: 4990, ResetAt: reset}, true,
		},
		{
			"gitlab epoch reset",
			http.Header{"Ratelimit-Limit": {"2000"}, "Ratelimit-Remaining": {"1999"}, "Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)}},
			ratelimit.Quota{Limit: 2000, Remaining: 1999, ResetAt: reset}, true,
		},
		{
			"gitlab reset time date",
			http.Header{"Ratelimit-Remaining": {"7"}, "Ratelimit-Resettime": {reset.Format(http.TimeFormat)}},
			ratelimit.Quota{Remaining: 7, ResetAt: reset}, true,
		},
		{"no headers", http.Header{}, ratelimit.Quota{}, false},
		{"remaining without reset", http.Header{"X-Ratelimit-Remaining": {"5"}}, ratelimit.Quota{}, false},
		{"garbage remaining", http.Header{"X-Ratelimit-Remaining": {"many"}, "X-Ratelimit-Reset": {"1"}}, ratelimit.Quota{}, false},
		{"garbage reset time", http.Header{"Ratelimit-Remaining": {"5"}, "Ratelimit-Resettime": {"soon"}}, ratelimit.Quota{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ratelimit.ParseHeaders(tt.header)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && (got.Limit != tt.want.Limit || got.Remaining != tt.want.Remaining || !got.ResetAt.Equal(tt.want.ResetAt)) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUpdateFromHeaders(t *testing.T) {
	now := epoch
	tr := newTracker(&now)
	key := ratelimit.NewKey("api.github.com", "tok")
	h := http.Header{
		"X-Ratelimit-Limit":     {"5000"},
		"X-Ratelimit-Remaining": {"100"},
		"X-Ratelimit-Reset":     {strconv.FormatInt(now.Add(time.Hour).Unix(), 10)},
	}
	if !tr.UpdateFromHeaders(key, h) {
		t.Fatal("headers should have been used")
	}
	if tr.UpdateFromHeaders(key, http.Header{}) {
		t.Fatal("empty headers must be ignored")
	}
	if q, _ := tr.Quota(key); q.Remaining != 100 {
		t.Errorf("remaining = %d, want 100", q.Remaining)
	}
}

func TestUpdateFromGraphQL(t *testing.T) {
	now := epoch
	tr := newTracker(&now)
	key := ratelimit.NewKey("api.github.com", "tok")
	tr.UpdateFromGraphQL(key, ratelimit.GraphQLInfo{})
	if _, ok := tr.Quota(key); ok {
		t.Fatal("info without resetAt must be ignored")
	}
	tr.UpdateFromGraphQL(key, ratelimit.GraphQLInfo{Cost: 1, Remaining: 3, Limit: 5000, ResetAt: now.Add(time.Hour)})
	if err := tr.Allow(key, 1); !errors.Is(err, gitprovider.ErrQuotaReserve) {
		t.Fatalf("GraphQL remaining 3 must be in reserve, got %v", err)
	}
}

func TestTrackerIsThreadSafe(_ *testing.T) {
	now := epoch
	tr := newTracker(&now)
	key := ratelimit.NewKey("h", "t")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.Set(key, ratelimit.Quota{Limit: 100, Remaining: i, ResetAt: epoch.Add(time.Hour)})
			_ = tr.Allow(key, 1)
		}(i)
	}
	wg.Wait()
}
