package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/store"
	"pdd/services/pkg/gitprovider/cache"
)

var epoch = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func openDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testKey() []byte { return bytes.Repeat([]byte{7}, 32) }

func newCipher(t *testing.T, key []byte) *store.Cipher {
	t.Helper()
	c, err := store.NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestOpenCreatesSchemaWithWAL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	db := openDB(t, dir)

	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	var timeout int
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 5000 {
		t.Fatalf("busy_timeout = %d, %v", timeout, err)
	}
	for _, table := range []string{"cache_entries", "provider_tokens"} {
		if _, err := db.Exec(`SELECT 1 FROM ` + table); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, store.DatabaseFile)); err != nil {
		t.Errorf("database file: %v", err)
	}
}

func TestOpenTwiceKeepsDataAndMigratesOnce(t *testing.T) {
	dir := t.TempDir()
	db := openDB(t, dir)
	if _, err := db.Exec(`INSERT INTO provider_tokens (account, ciphertext, updated_at) VALUES ('a', x'00', 1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db = openDB(t, dir)
	var rows, versions int
	_ = db.QueryRow(`SELECT COUNT(*) FROM provider_tokens`).Scan(&rows)
	_ = db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&versions)
	if rows != 1 || versions != 2 {
		t.Fatalf("rows = %d, schema versions = %d", rows, versions)
	}
}

func TestOpenFailsWhenDirIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), file); err == nil {
		t.Fatal("Open on a file path succeeded")
	}
}

func TestCipherRoundTrip(t *testing.T) {
	c := newCipher(t, testKey())
	sealed, err := c.Seal([]byte("gho_secret"), []byte("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("gho_secret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	again, _ := c.Seal([]byte("gho_secret"), []byte("alice"))
	if bytes.Equal(sealed, again) {
		t.Fatal("two encryptions of the same value are identical (nonce reused)")
	}

	plain, err := c.Open(sealed, []byte("alice"))
	if err != nil || string(plain) != "gho_secret" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
}

func TestCipherRejects(t *testing.T) {
	c := newCipher(t, testKey())
	sealed, _ := c.Seal([]byte("token"), []byte("alice"))
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	otherKey := newCipher(t, bytes.Repeat([]byte{8}, 32))

	tests := map[string]func() error{
		"other account": func() error { _, err := c.Open(sealed, []byte("bob")); return err },
		"tampered":      func() error { _, err := c.Open(tampered, []byte("alice")); return err },
		"other key":     func() error { _, err := otherKey.Open(sealed, []byte("alice")); return err },
		"too short":     func() error { _, err := c.Open([]byte{1, 2}, []byte("alice")); return err },
	}
	for name, open := range tests {
		if err := open(); !errors.Is(err, store.ErrDecrypt) {
			t.Errorf("%s: error = %v, want ErrDecrypt", name, err)
		}
	}
}

func TestNewCipherRequires32ByteKey(t *testing.T) {
	if _, err := store.NewCipher(make([]byte, 16)); err == nil {
		t.Fatal("16-byte key accepted")
	}
}

func TestTokenStoreSaveLoadUpsert(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, t.TempDir())
	tokens := store.NewTokenStore(db, newCipher(t, testKey()), func() time.Time { return epoch })

	if _, err := tokens.Load(ctx, "alice"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("Load unknown = %v, want ErrTokenNotFound", err)
	}
	for _, token := range []string{"first", "second"} {
		if err := tokens.Save(ctx, "alice", token); err != nil {
			t.Fatal(err)
		}
	}
	got, err := tokens.Load(ctx, "alice")
	if err != nil || got != "second" {
		t.Fatalf("Load = %q, %v", got, err)
	}
}

func TestTokenStoreEncryptsAtRest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openDB(t, dir)
	tokens := store.NewTokenStore(db, newCipher(t, testKey()), nil)
	if err := tokens.Save(ctx, "alice", "gho_plain_secret"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := db.QueryRow(`SELECT ciphertext FROM provider_tokens WHERE account = 'alice'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("gho_plain_secret")) {
		t.Fatal("token stored in plaintext")
	}
	_ = db.Close()
	for _, name := range []string{store.DatabaseFile, store.DatabaseFile + "-wal"} {
		if content, err := os.ReadFile(filepath.Join(dir, name)); err == nil && bytes.Contains(content, []byte("gho_plain_secret")) {
			t.Fatalf("%s contains the plaintext token", name)
		}
	}
}

func TestTokenStoreSurvivesRestartAndFailsWithWrongKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openDB(t, dir)
	if err := store.NewTokenStore(db, newCipher(t, testKey()), nil).Save(ctx, "alice", "tok"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db = openDB(t, dir)
	got, err := store.NewTokenStore(db, newCipher(t, testKey()), nil).Load(ctx, "alice")
	if err != nil || got != "tok" {
		t.Fatalf("after restart Load = %q, %v", got, err)
	}
	wrong := store.NewTokenStore(db, newCipher(t, bytes.Repeat([]byte{9}, 32)), nil)
	if _, err := wrong.Load(ctx, "alice"); !errors.Is(err, store.ErrDecrypt) {
		t.Fatalf("Load with wrong key = %v, want ErrDecrypt", err)
	}
}

func entry(key string, ttl, stale time.Duration) cache.Entry {
	return cache.Entry{
		Key: key, Value: []byte(`{"data":1}`), ETag: `"v1"`, FetchedAt: epoch,
		FreshUntil: epoch.Add(ttl), EvictAfter: epoch.Add(ttl + stale),
	}
}

func TestCacheStoreSetGetDelete(t *testing.T) {
	ctx := context.Background()
	s := store.NewCacheStore(openDB(t, t.TempDir()), func() time.Time { return epoch })

	if _, ok, err := s.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("Get missing = %v, %v", ok, err)
	}
	want := entry("k", time.Minute, time.Hour)
	if err := s.Set(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if string(got.Value) != string(want.Value) || got.ETag != want.ETag || got.Key != "k" ||
		!got.FetchedAt.Equal(want.FetchedAt) || !got.FreshUntil.Equal(want.FreshUntil) || !got.EvictAfter.Equal(want.EvictAfter) {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}

	replaced := want
	replaced.Value = []byte("new")
	if err := s.Set(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Get(ctx, "k"); string(got.Value) != "new" {
		t.Fatalf("value after replace = %q", got.Value)
	}

	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "k"); ok {
		t.Fatal("entry still present after Delete")
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
}

func TestCacheStoreExpiredEntryIsMissing(t *testing.T) {
	ctx := context.Background()
	now := epoch
	s := store.NewCacheStore(openDB(t, t.TempDir()), func() time.Time { return now })
	if err := s.Set(ctx, entry("k", time.Minute, time.Hour)); err != nil {
		t.Fatal(err)
	}

	now = epoch.Add(30 * time.Minute)
	got, ok, err := s.Get(ctx, "k")
	if err != nil || !ok || got.Fresh(now) {
		t.Fatalf("stale entry: ok=%v err=%v fresh=%v", ok, err, got.Fresh(now))
	}
	now = epoch.Add(2 * time.Hour)
	if _, ok, err := s.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("expired entry: ok=%v err=%v", ok, err)
	}
}

func TestCacheStoreSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openDB(t, dir)
	if err := store.NewCacheStore(db, func() time.Time { return epoch }).Set(ctx, entry("k", time.Minute, time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	reopened := store.NewCacheStore(openDB(t, dir), func() time.Time { return epoch })
	got, ok, err := reopened.Get(ctx, "k")
	if err != nil || !ok || string(got.Value) != `{"data":1}` {
		t.Fatalf("after restart Get = %+v, %v, %v", got, ok, err)
	}
}

func TestCacheStorePurgeExpired(t *testing.T) {
	ctx := context.Background()
	now := epoch
	s := store.NewCacheStore(openDB(t, t.TempDir()), func() time.Time { return now })
	_ = s.Set(ctx, entry("short", time.Minute, time.Minute))
	_ = s.Set(ctx, entry("long", time.Minute, 24*time.Hour))

	now = epoch.Add(time.Hour)
	n, err := s.PurgeExpired(ctx)
	if err != nil || n != 1 {
		t.Fatalf("PurgeExpired = %d, %v; want 1", n, err)
	}
	if _, ok, _ := s.Get(ctx, "long"); !ok {
		t.Fatal("unexpired entry was purged")
	}
}

func TestStoresReportClosedDatabase(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, t.TempDir())
	_ = db.Close()
	cs := store.NewCacheStore(db, nil)
	ts := store.NewTokenStore(db, newCipher(t, testKey()), nil)

	if _, _, err := cs.Get(ctx, "k"); err == nil {
		t.Error("Get on closed db succeeded")
	}
	if err := cs.Set(ctx, entry("k", time.Minute, time.Minute)); err == nil {
		t.Error("Set on closed db succeeded")
	}
	if err := cs.Delete(ctx, "k"); err == nil {
		t.Error("Delete on closed db succeeded")
	}
	if _, err := cs.PurgeExpired(ctx); err == nil {
		t.Error("PurgeExpired on closed db succeeded")
	}
	if err := ts.Save(ctx, "a", "t"); err == nil {
		t.Error("Save on closed db succeeded")
	}
	if _, err := ts.Load(ctx, "a"); err == nil || errors.Is(err, store.ErrTokenNotFound) {
		t.Errorf("Load on closed db = %v", err)
	}
}
