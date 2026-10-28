package pulls

import (
	"context"
	"fmt"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/repos"
)

// FilesResult is the changed files of a pull request with the commits they were computed for.
type FilesResult struct {
	HeadSHA, BaseSHA string
	Files            []model.ChangedFile
	// Truncated is true when files or patches were left out.
	Truncated bool
	// Stale is true when the commits or the files were served from stale cache.
	Stale     bool
	FetchedAt time.Time
}

// PullFiles loads the files changed by a pull request. Patches are dropped, from the first one
// that does not fit, once their total exceeds the configured patch budget.
func (s *Pulls) PullFiles(ctx context.Context, token string, repo model.Repo, number int) (FilesResult, error) {
	adapter, err := s.adapter(repo)
	if err != nil {
		return FilesResult{}, err
	}
	detail, detailMeta, err := adapter.PullDetail(ctx, token, repo, number)
	if err != nil {
		return FilesResult{}, err
	}
	changed, filesMeta, err := adapter.PullFiles(ctx, token, repo, number)
	if err != nil {
		return FilesResult{}, err
	}
	files, trimmed := s.capPatches(changed.Files)
	meta := detailMeta.Merge(filesMeta)
	return FilesResult{
		HeadSHA: detail.HeadSHA, BaseSHA: detail.BaseSHA, Files: files, Truncated: changed.Truncated || trimmed,
		Stale: meta.Stale, FetchedAt: s.fetchedAt(meta),
	}, nil
}

func (s *Pulls) capPatches(files []model.ChangedFile) ([]model.ChangedFile, bool) {
	if s.maxPatchBytes <= 0 {
		return files, false
	}
	var used int64
	trimmed := false
	for i := range files {
		size := int64(len(files[i].Patch))
		if used+size > s.maxPatchBytes {
			files[i].Patch, trimmed = "", true
			continue
		}
		used += size
	}
	return files, trimmed
}

// PullTree loads the repository tree at the head commit of a pull request.
func (s *Pulls) PullTree(ctx context.Context, token string, repo model.Repo, number int) (repos.TreeResult, error) {
	adapter, err := s.adapter(repo)
	if err != nil {
		return repos.TreeResult{}, err
	}
	detail, _, err := adapter.PullDetail(ctx, token, repo, number)
	if err != nil {
		return repos.TreeResult{}, err
	}
	if detail.HeadSHA == "" {
		return repos.TreeResult{}, fmt.Errorf("service: provider returned no head commit for pull request %d", number)
	}
	return s.repos.Tree(ctx, token, repo, detail.HeadSHA)
}

func (s *Pulls) adapter(repo model.Repo) (Provider, error) {
	adapter := s.providers[repo.Provider]
	if adapter == nil {
		return nil, fmt.Errorf("service: no adapter for provider %q", repo.Provider)
	}
	return adapter, nil
}
