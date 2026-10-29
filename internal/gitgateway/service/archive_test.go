package service_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/archive"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/service"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func archiveService(t *testing.T, p provider.Provider, cacheMax, maxBytes int64) *service.Service {
	t.Helper()
	cache, err := archive.Open(filepath.Join(t.TempDir(), "archives"), cacheMax, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service.New(map[model.Provider]provider.Provider{model.GitHub: p}, nil,
		service.Limits{ArchiveBytes: maxBytes}, service.WithArchiveCache(cache))
}

func body(content string) func() (provider.ArchiveStream, error) {
	return func() (provider.ArchiveStream, error) {
		return provider.ArchiveStream{Body: io.NopCloser(bytes.NewReader([]byte(content))), Size: int64(len(content))}, nil
	}
}

func readArchive(t *testing.T, res service.ArchiveResult) string {
	t.Helper()
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestArchiveDownloadsOnceAndServesFromTheCache(t *testing.T) {
	p := &fakeProvider{archive: body("tar-gz-bytes")}
	svc := archiveService(t, p, 1000, 100)
	repo := model.Repo{Provider: model.GitHub, Owner: "o", Name: "n"}

	for range 2 {
		res, err := svc.Archive(context.Background(), "tok", repo, sha)
		if err != nil {
			t.Fatal(err)
		}
		if got := readArchive(t, res); got != "tar-gz-bytes" || res.Size != 12 {
			t.Errorf("archive %q size %d", got, res.Size)
		}
	}

	if p.archiveCalls != 1 || p.gotRef != sha {
		t.Errorf("provider called %d times with ref %q", p.archiveCalls, p.gotRef)
	}
}

func TestArchiveCacheIsKeyedByRepositoryAndCommit(t *testing.T) {
	p := &fakeProvider{archive: body("x")}
	svc := archiveService(t, p, 1000, 100)
	a := model.Repo{Provider: model.GitHub, Owner: "o", Name: "a"}
	b := model.Repo{Provider: model.GitHub, Owner: "o", Name: "b"}

	for _, tc := range []struct {
		repo model.Repo
		ref  string
	}{{a, sha}, {b, sha}, {a, "f" + sha[1:]}} {
		res, err := svc.Archive(context.Background(), "tok", tc.repo, tc.ref)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
	}

	if p.archiveCalls != 3 {
		t.Errorf("provider called %d times, want 3", p.archiveCalls)
	}
}

func TestArchiveRefusesAnAnnouncedSizeOverTheLimitWithoutReading(t *testing.T) {
	read := false
	p := &fakeProvider{archive: func() (provider.ArchiveStream, error) {
		return provider.ArchiveStream{Body: readFlag{&read}, Size: 101}, nil
	}}
	svc := archiveService(t, p, 1000, 100)

	_, err := svc.Archive(context.Background(), "tok", ghRepo, sha)

	if !errors.Is(err, service.ErrArchiveTooLarge) || read {
		t.Errorf("err %v, body read %v", err, read)
	}
}

type readFlag struct{ read *bool }

func (r readFlag) Read([]byte) (int, error) { *r.read = true; return 0, io.EOF }
func (readFlag) Close() error               { return nil }

func TestArchiveRefusesAnUnannouncedSizeOverTheLimitAndDoesNotCacheIt(t *testing.T) {
	p := &fakeProvider{archive: func() (provider.ArchiveStream, error) {
		return provider.ArchiveStream{Body: io.NopCloser(bytes.NewReader(make([]byte, 101))), Size: -1}, nil
	}}
	svc := archiveService(t, p, 1000, 100)

	for range 2 {
		if _, err := svc.Archive(context.Background(), "tok", ghRepo, sha); !errors.Is(err, service.ErrArchiveTooLarge) {
			t.Fatalf("err = %v", err)
		}
	}

	if p.archiveCalls != 2 {
		t.Errorf("a refused archive was cached, provider calls = %d", p.archiveCalls)
	}
}

func TestArchiveProviderErrorsPassThrough(t *testing.T) {
	p := &fakeProvider{archive: func() (provider.ArchiveStream, error) { return provider.ArchiveStream{}, provider.ErrNotFound }}

	_, err := archiveService(t, p, 1000, 100).Archive(context.Background(), "tok", ghRepo, sha)

	if !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestArchiveWithoutAdapterOrCacheFails(t *testing.T) {
	if _, err := service.New(nil, nil, service.Limits{}).Archive(context.Background(), "t", ghRepo, sha); err == nil {
		t.Error("Archive without adapter did not fail")
	}
	p := &fakeProvider{archive: body("x")}
	svc := service.New(map[model.Provider]provider.Provider{model.GitHub: p}, nil, service.Limits{})
	if _, err := svc.Archive(context.Background(), "t", ghRepo, sha); err == nil || p.archiveCalls != 0 {
		t.Errorf("Archive without cache: err %v, provider calls %d", err, p.archiveCalls)
	}
}

func TestArchiveSharesOneDownloadBetweenConcurrentCallers(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	var mu sync.Mutex
	calls := 0
	p := &fakeProvider{archive: func() (provider.ArchiveStream, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		started <- struct{}{}
		<-release
		return body("shared")()
	}}
	svc := archiveService(t, p, 1000, 100)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.Archive(context.Background(), "tok", ghRepo, sha)
			if err != nil {
				t.Error(err)
				return
			}
			if got := readArchive(t, res); got != "shared" {
				t.Errorf("archive = %q", got)
			}
		}()
	}
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("provider called %d times, want 1", calls)
	}
}

func TestArchiveCallerStartsOverWhenTheLeaderIsCancelled(t *testing.T) {
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderStarted := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	p := &fakeProvider{}
	p.archive = func() (provider.ArchiveStream, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(leaderStarted)
			<-leaderCtx.Done()
			return provider.ArchiveStream{}, leaderCtx.Err()
		}
		return body("second")()
	}
	svc := archiveService(t, p, 1000, 100)

	go func() { _, _ = svc.Archive(leaderCtx, "tok", ghRepo, sha) }()
	<-leaderStarted
	done := make(chan string, 1)
	go func() {
		res, err := svc.Archive(context.Background(), "tok", ghRepo, sha)
		if err != nil {
			done <- err.Error()
			return
		}
		done <- readArchive(t, res)
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()

	select {
	case got := <-done:
		if got != "second" {
			t.Errorf("second caller got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second caller did not finish")
	}
}
