package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"pdd/services/internal/gitgateway/archive"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
)

// ErrArchiveTooLarge is matched by errors.Is when the archive exceeds Limits.ArchiveBytes.
var ErrArchiveTooLarge = archive.ErrTooLarge

// archiveAttempts bounds how often Archive starts over when a download it waited for failed
// on behalf of another caller or the stored archive was evicted before it could be opened.
const archiveAttempts = 3

// ArchiveResult is a source archive (tar.gz) served from the disk cache.
type ArchiveResult struct {
	// Body is the archive; the caller closes it.
	Body io.ReadCloser
	// Size is the length of the archive in bytes.
	Size int64
}

// Archive returns repo at ref (a commit SHA) as a tar.gz. A cached archive is served without
// asking the provider; otherwise it is downloaded once, however many callers wait for it,
// and stored within the cache limits.
func (s *Service) Archive(ctx context.Context, token string, repo model.Repo, ref string) (ArchiveResult, error) {
	adapter, err := s.adapter(repo)
	if err != nil {
		return ArchiveResult{}, err
	}
	if s.archives == nil {
		return ArchiveResult{}, errors.New("service: source archives are not configured")
	}
	key := archive.Key(string(repo.Provider), repo.BaseURL, repo.FullPath(), ref)
	for range archiveAttempts {
		file, size, ok, err := s.archives.Get(key)
		if err != nil {
			return ArchiveResult{}, err
		}
		if ok {
			return ArchiveResult{Body: file, Size: size}, nil
		}
		_, err, _ = s.flight.Do(key, func() (any, error) { return nil, s.download(ctx, adapter, token, repo, ref, key) })
		// A shared download led by another caller may have been cancelled by that caller;
		// this caller is still interested, so it starts over.
		if err != nil && (!errors.Is(err, context.Canceled) || ctx.Err() != nil) {
			return ArchiveResult{}, err
		}
	}
	return ArchiveResult{}, fmt.Errorf("service: archive of %s did not stay cached", repo.FullPath())
}

func (s *Service) download(ctx context.Context, adapter provider.Provider, token string, repo model.Repo, ref, key string) error {
	limit := s.limits.ArchiveBytes
	if limit <= 0 {
		limit = math.MaxInt64 - 1
	}
	stream, err := adapter.Archive(ctx, token, repo, ref)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.Size > limit {
		return fmt.Errorf("service: archive of %s is %d bytes, limit %d: %w", repo.FullPath(), stream.Size, limit, ErrArchiveTooLarge)
	}
	if _, err := s.archives.Put(key, stream.Body, limit); err != nil {
		return fmt.Errorf("service: archive of %s: %w", repo.FullPath(), err)
	}
	return nil
}
