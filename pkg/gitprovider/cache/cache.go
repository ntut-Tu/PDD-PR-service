// Package cache defines the response cache used by the Git provider client and
// an in-memory implementation.
//
// An entry has two lifetimes: it is fresh until FreshUntil and may still be
// returned as stale data until EvictAfter. Stale entries are what the client
// serves when the quota is in reserve or the provider keeps failing, and what it
// revalidates with If-None-Match.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Entry is one cached response body.
type Entry struct {
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
func (e Entry) Fresh(now time.Time) bool { return now.Before(e.FreshUntil) }

// Expired reports whether the entry is past its stale lifetime.
func (e Entry) Expired(now time.Time) bool { return !now.Before(e.EvictAfter) }

// Store persists entries. Implementations must be safe for concurrent use and
// must not return expired entries. A missing key is not an error.
type Store interface {
	// Get returns the entry for key and whether it exists.
	Get(ctx context.Context, key string) (Entry, bool, error)
	// Set inserts or replaces the entry stored under e.Key.
	Set(ctx context.Context, e Entry) error
	// Delete removes key; deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// Key builds a fixed-length cache key from the parts that identify a request,
// such as provider host, token fingerprint, URL or GraphQL query and variables.
func Key(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// MemoryStore is a thread-safe in-memory Store with a bounded entry count.
type MemoryStore struct {
	mu         sync.Mutex
	entries    map[string]Entry
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
	return &MemoryStore{entries: make(map[string]Entry), maxEntries: maxEntries, now: now}
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, key string) (Entry, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok {
		return Entry{}, false, nil
	}
	if e.Expired(s.now()) {
		delete(s.entries, key)
		return Entry{}, false, nil
	}
	return e, true, nil
}

// Set implements Store. When the store is full, expired entries are dropped
// first, then the entry with the oldest FetchedAt.
func (s *MemoryStore) Set(_ context.Context, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[e.Key]; !exists && len(s.entries) >= s.maxEntries {
		s.evict()
	}
	s.entries[e.Key] = e
	return nil
}

// Delete implements Store.
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
