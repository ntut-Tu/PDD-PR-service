package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// CacheEntry is one cached response body.
type CacheEntry struct {
	Key   string
	Value []byte
	// ETag is the validator returned by the provider, empty when none.
	ETag string
	// FetchedAt is when the data was last confirmed by the provider.
	FetchedAt time.Time
	// FreshUntil ends the period in which the entry is served without a request.
	FreshUntil time.Time
	// EvictAfter ends the period in which the entry may be served as stale.
	EvictAfter time.Time
}

// Fresh reports whether the entry may be served without contacting the provider.
func (e CacheEntry) Fresh(now time.Time) bool { return now.Before(e.FreshUntil) }

// Expired reports whether the entry is past its stale lifetime.
func (e CacheEntry) Expired(now time.Time) bool { return !now.Before(e.EvictAfter) }

// ResponseCache persists entries. Implementations must be safe for concurrent use and
// must not return expired entries. A missing key is not an error.
type ResponseCache interface {
	// Get returns the entry for key and whether it exists.
	Get(ctx context.Context, key string) (CacheEntry, bool, error)
	// Set inserts or replaces the entry stored under e.Key.
	Set(ctx context.Context, e CacheEntry) error
}

// CacheKey builds a fixed-length cache key from the parts that identify a request,
// such as provider host, token fingerprint, URL or GraphQL query and variables.
func CacheKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// MemoryStore is a thread-safe in-memory ResponseCache with a bounded entry count.
type MemoryStore struct {
	mu         sync.Mutex
	entries    map[string]CacheEntry
	maxEntries int
	now        func() time.Time
}

// NewMemoryStore creates a MemoryStore holding at most maxEntries entries
// (values below 1 mean 1024). A nil now defaults to time.Now.
func NewMemoryStore(maxEntries int, now func() time.Time) *MemoryStore {
	if maxEntries < 1 {
		maxEntries = 1024
	}
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{entries: make(map[string]CacheEntry), maxEntries: maxEntries, now: now}
}

// Get implements ResponseCache.
func (s *MemoryStore) Get(_ context.Context, key string) (CacheEntry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return CacheEntry{}, false, nil
	}
	if e.Expired(s.now()) {
		delete(s.entries, key)
		return CacheEntry{}, false, nil
	}
	return e, true, nil
}

// Set implements ResponseCache. When the store is full, expired entries are dropped
// first, then the entry with the oldest FetchedAt.
func (s *MemoryStore) Set(_ context.Context, e CacheEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[e.Key]; !exists && len(s.entries) >= s.maxEntries {
		s.evict()
	}
	s.entries[e.Key] = e
	return nil
}

// Delete removes a cached response. A missing key is not an error.
func (s *MemoryStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	return nil
}

func (s *MemoryStore) evict() {
	now := s.now()
	var oldestKey string
	var oldest time.Time
	for k, e := range s.entries {
		if e.Expired(now) {
			delete(s.entries, k)
			continue
		}
		if oldestKey == "" || e.FetchedAt.Before(oldest) {
			oldestKey, oldest = k, e.FetchedAt
		}
	}
	if len(s.entries) >= s.maxEntries {
		delete(s.entries, oldestKey)
	}
}
