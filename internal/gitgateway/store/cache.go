package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"pdd/services/pkg/gitprovider/cache"
)

// CacheStore is a SQLite implementation of cache.Store.
type CacheStore struct {
	db  *sql.DB
	now func() time.Time
}

var _ cache.Store = (*CacheStore)(nil)

// NewCacheStore creates a CacheStore on db. A nil now defaults to time.Now.
func NewCacheStore(db *sql.DB, now func() time.Time) *CacheStore {
	if now == nil {
		now = time.Now
	}
	return &CacheStore{db: db, now: now}
}

// Get implements cache.Store. Expired entries are deleted and reported missing.
func (s *CacheStore) Get(ctx context.Context, key string) (cache.Entry, bool, error) {
	var (
		e                               cache.Entry
		fetched, freshUntil, evictAfter int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT value, etag, fetched_at, fresh_until, evict_after FROM cache_entries WHERE key = ?`, key).
		Scan(&e.Value, &e.ETag, &fetched, &freshUntil, &evictAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return cache.Entry{}, false, nil
	}
	if err != nil {
		return cache.Entry{}, false, fmt.Errorf("store: read cache entry: %w", err)
	}
	e.Key = key
	e.FetchedAt = time.Unix(0, fetched).UTC()
	e.FreshUntil = time.Unix(0, freshUntil).UTC()
	e.EvictAfter = time.Unix(0, evictAfter).UTC()
	if e.Expired(s.now()) {
		return cache.Entry{}, false, s.Delete(ctx, key)
	}
	return e, true, nil
}

// Set implements cache.Store.
func (s *CacheStore) Set(ctx context.Context, e cache.Entry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cache_entries (key, value, etag, fetched_at, fresh_until, evict_after)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, etag = excluded.etag,
		   fetched_at = excluded.fetched_at, fresh_until = excluded.fresh_until, evict_after = excluded.evict_after`,
		e.Key, e.Value, e.ETag, e.FetchedAt.UnixNano(), e.FreshUntil.UnixNano(), e.EvictAfter.UnixNano())
	if err != nil {
		return fmt.Errorf("store: write cache entry: %w", err)
	}
	return nil
}

// Delete implements cache.Store.
func (s *CacheStore) Delete(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete cache entry: %w", err)
	}
	return nil
}

// PurgeExpired removes every entry past its stale lifetime and returns how many.
func (s *CacheStore) PurgeExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cache_entries WHERE evict_after <= ?`, s.now().UnixNano())
	if err != nil {
		return 0, fmt.Errorf("store: purge cache: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge cache: %w", err)
	}
	return n, nil
}
