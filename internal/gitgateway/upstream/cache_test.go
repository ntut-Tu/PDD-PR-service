package upstream_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/upstream"
)

func entry(key string, fetched time.Time) upstream.CacheEntry {
	return upstream.CacheEntry{
		Key: key, Value: []byte("v-" + key), ETag: `"e"`,
		FetchedAt: fetched, FreshUntil: fetched.Add(time.Minute), EvictAfter: fetched.Add(time.Hour),
	}
}

func TestEntryFreshness(t *testing.T) {
	e := entry("k", epoch)
	if !e.Fresh(epoch.Add(59 * time.Second)) {
		t.Error("entry should be fresh before FreshUntil")
	}
	if e.Fresh(epoch.Add(time.Minute)) {
		t.Error("entry should be stale at FreshUntil")
	}
	if e.Expired(epoch.Add(59 * time.Minute)) {
		t.Error("stale entry is not expired before EvictAfter")
	}
	if !e.Expired(epoch.Add(time.Hour)) {
		t.Error("entry should be expired at EvictAfter")
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	now := epoch
	s := upstream.NewMemoryStore(10, func() time.Time { return now })
	ctx := context.Background()
	if _, ok, err := s.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	if err := s.Set(ctx, entry("k", now)); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.Get(ctx, "k")
	if !ok || string(got.Value) != "v-k" || got.ETag != `"e"` {
		t.Fatalf("unexpected entry %+v ok=%v", got, ok)
	}
	now = now.Add(30 * time.Minute)
	if got, ok, _ = s.Get(ctx, "k"); !ok || got.Fresh(now) {
		t.Error("entry should still be returned as stale")
	}
	now = now.Add(time.Hour)
	if _, ok, _ = s.Get(ctx, "k"); ok {
		t.Error("expired entry must not be returned")
	}
}

func TestMemoryStoreDelete(t *testing.T) {
	s := upstream.NewMemoryStore(10, func() time.Time { return epoch })
	ctx := context.Background()
	_ = s.Set(ctx, entry("k", epoch))
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "missing"); err != nil {
		t.Fatalf("deleting a missing key must not fail: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Error("entry should be deleted")
	}
}

func TestMemoryStoreEvictsOldestWhenFull(t *testing.T) {
	s := upstream.NewMemoryStore(2, func() time.Time { return epoch })
	ctx := context.Background()
	_ = s.Set(ctx, entry("old", epoch.Add(-time.Minute)))
	_ = s.Set(ctx, entry("new", epoch))
	_ = s.Set(ctx, entry("newest", epoch.Add(time.Second)))
	if _, ok, _ := s.Get(ctx, "old"); ok {
		t.Error("oldest entry should have been evicted")
	}
	for _, k := range []string{"new", "newest"} {
		if _, ok, _ := s.Get(ctx, k); !ok {
			t.Errorf("%s should be kept", k)
		}
	}
}

func TestMemoryStoreEvictsExpiredFirst(t *testing.T) {
	now := epoch
	s := upstream.NewMemoryStore(2, func() time.Time { return now })
	ctx := context.Background()
	_ = s.Set(ctx, entry("expiring", epoch))
	_ = s.Set(ctx, entry("later", epoch.Add(2*time.Hour)))
	now = epoch.Add(90 * time.Minute)
	_ = s.Set(ctx, entry("fresh", now))
	if _, ok, _ := s.Get(ctx, "later"); !ok {
		t.Error("a live entry must survive when an expired one can be dropped")
	}
}

func TestMemoryStoreReplaceDoesNotEvict(t *testing.T) {
	s := upstream.NewMemoryStore(1, func() time.Time { return epoch })
	ctx := context.Background()
	_ = s.Set(ctx, entry("k", epoch))
	e := entry("k", epoch)
	e.Value = []byte("replaced")
	_ = s.Set(ctx, e)
	if got, ok, _ := s.Get(ctx, "k"); !ok || string(got.Value) != "replaced" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestMemoryStoreDefaults(t *testing.T) {
	s := upstream.NewMemoryStore(0, nil)
	e := entry("k", time.Now())
	_ = s.Set(context.Background(), e)
	if _, ok, _ := s.Get(context.Background(), "k"); !ok {
		t.Error("default store should hold entries")
	}
}

func TestMemoryStoreIsThreadSafe(_ *testing.T) {
	s := upstream.NewMemoryStore(5, func() time.Time { return epoch })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := strconv.Itoa(i % 8)
			_ = s.Set(context.Background(), entry(k, epoch))
			_, _, _ = s.Get(context.Background(), k)
			_ = s.Delete(context.Background(), k)
		}(i)
	}
	wg.Wait()
}

func TestKey(t *testing.T) {
	a := upstream.CacheKey("rest", "host", "https://x/y")
	if a != upstream.CacheKey("rest", "host", "https://x/y") {
		t.Error("key must be deterministic")
	}
	if a == upstream.CacheKey("rest", "hos", "that/https://x/y") || upstream.CacheKey("ab", "c") == upstream.CacheKey("a", "bc") {
		t.Error("parts must not be ambiguous")
	}
	if len(a) != 64 {
		t.Errorf("key length = %d, want 64", len(a))
	}
}
