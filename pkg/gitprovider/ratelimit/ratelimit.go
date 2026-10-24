// Package ratelimit tracks the remaining API quota of GitHub and GitLab tokens.
//
// Quota is learned from response data only: the REST headers
// (x-ratelimit-* for GitHub, RateLimit-* for GitLab) and the GraphQL
// rateLimit field of GitHub. Raw tokens are never stored; a Key holds a
// SHA-256 fingerprint of the token.
package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"pdd/services/pkg/gitprovider"
)

// Key identifies one quota window: a provider host and a token fingerprint.
type Key struct {
	Host      string
	TokenHash string
}

// NewKey builds a Key from a provider host and a raw token. The token is hashed
// and the raw value is not retained.
func NewKey(host, token string) Key {
	sum := sha256.Sum256([]byte(token))
	return Key{Host: strings.ToLower(host), TokenHash: hex.EncodeToString(sum[:8])}
}

// Quota is the last known state of a quota window.
type Quota struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// GraphQLInfo mirrors the GitHub GraphQL rateLimit object.
type GraphQLInfo struct {
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	ResetAt   time.Time `json:"resetAt"`
}

// Config sets the reserve threshold.
type Config struct {
	// ReserveFraction is the share of the window limit kept in reserve, e.g. 0.1.
	ReserveFraction float64
	// ReserveMin is the lowest reserve in requests, used when the fraction rounds
	// to less or the limit is unknown.
	ReserveMin int
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// DefaultConfig returns the defaults documented in the ADR: 10% of the window,
// at least 10 requests.
func DefaultConfig() Config {
	return Config{ReserveFraction: 0.1, ReserveMin: 10}
}

// Tracker is a thread-safe quota tracker.
type Tracker struct {
	cfg    Config
	mu     sync.Mutex
	quotas map[Key]Quota
}

// NewTracker creates a Tracker. A nil cfg.Now defaults to time.Now.
func NewTracker(cfg Config) *Tracker {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Tracker{cfg: cfg, quotas: make(map[Key]Quota)}
}

// Allow reports whether a request of the given cost may be sent. It returns a
// *gitprovider.QuotaReserveError when the remaining quota after the request
// would drop below the reserve. Unknown or expired windows are allowed.
func (t *Tracker) Allow(key Key, cost int) error {
	if cost < 1 {
		cost = 1
	}
	q, ok := t.Quota(key)
	if !ok {
		return nil
	}
	reserve := t.reserve(q.Limit)
	if q.Remaining-cost < reserve {
		return &gitprovider.QuotaReserveError{Remaining: q.Remaining, Reserve: reserve, ResetAt: q.ResetAt}
	}
	return nil
}

// Quota returns the last known quota of key. It reports false when nothing is
// known or the window has already been reset.
func (t *Tracker) Quota(key Key) (Quota, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	q, ok := t.quotas[key]
	if !ok {
		return Quota{}, false
	}
	if !q.ResetAt.After(t.cfg.Now()) {
		delete(t.quotas, key)
		return Quota{}, false
	}
	return q, true
}

// Set records a quota window.
func (t *Tracker) Set(key Key, q Quota) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.quotas[key] = q
}

// UpdateFromHeaders records the quota advertised by a REST response. It
// understands the GitHub x-ratelimit-limit/remaining/reset headers and the
// GitLab RateLimit-Limit/Remaining/Reset (epoch seconds) or RateLimit-ResetTime
// (HTTP date) headers. It reports false when the response carries no usable quota.
func (t *Tracker) UpdateFromHeaders(key Key, h http.Header) bool {
	q, ok := ParseHeaders(h)
	if !ok {
		return false
	}
	t.Set(key, q)
	return true
}

// UpdateFromGraphQL records the quota reported by the GitHub GraphQL rateLimit field.
func (t *Tracker) UpdateFromGraphQL(key Key, info GraphQLInfo) {
	if info.ResetAt.IsZero() {
		return
	}
	t.Set(key, Quota{Limit: info.Limit, Remaining: info.Remaining, ResetAt: info.ResetAt})
}

// ParseHeaders extracts a Quota from REST response headers.
func ParseHeaders(h http.Header) (Quota, bool) {
	remaining, ok := intHeader(h, "X-Ratelimit-Remaining", "Ratelimit-Remaining")
	if !ok {
		return Quota{}, false
	}
	reset, ok := resetHeader(h)
	if !ok {
		return Quota{}, false
	}
	limit, _ := intHeader(h, "X-Ratelimit-Limit", "Ratelimit-Limit")
	return Quota{Limit: limit, Remaining: remaining, ResetAt: reset}, true
}

func (t *Tracker) reserve(limit int) int {
	fraction := int(math.Ceil(float64(limit) * t.cfg.ReserveFraction))
	if fraction < t.cfg.ReserveMin {
		return t.cfg.ReserveMin
	}
	return fraction
}

func intHeader(h http.Header, names ...string) (int, bool) {
	for _, name := range names {
		if v := h.Get(name); v != "" {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			return n, err == nil
		}
	}
	return 0, false
}

func resetHeader(h http.Header) (time.Time, bool) {
	if secs, ok := intHeader(h, "X-Ratelimit-Reset", "Ratelimit-Reset"); ok {
		return time.Unix(int64(secs), 0), true
	}
	if v := h.Get("Ratelimit-Resettime"); v != "" {
		if ts, err := http.ParseTime(v); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}
