package service

import (
	"bytes"
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
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

// TreeResult is a repository tree at a commit.
type TreeResult struct {
	Ref       string
	Entries   []model.TreeEntry
	Truncated bool
	Stale     bool
	FetchedAt time.Time
}

// FileResult is one file at a commit.
type FileResult struct {
	Ref, Path string
	// Size is the size of the whole file in bytes.
	Size int64
	// Content is the UTF-8 text, cut at Limits.FileBytes; empty for binary files.
	Content   string
	Binary    bool
	Truncated bool
}

// PullFiles loads the files changed by a pull request. Patches are dropped, from the first one
// that does not fit, once their total exceeds Limits.PatchBytes.
func (s *Service) PullFiles(ctx context.Context, token string, repo model.Repo, number int) (FilesResult, error) {
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

func (s *Service) capPatches(files []model.ChangedFile) ([]model.ChangedFile, bool) {
	if s.limits.PatchBytes <= 0 {
		return files, false
	}
	var used int64
	trimmed := false
	for i := range files {
		size := int64(len(files[i].Patch))
		if used+size > s.limits.PatchBytes {
			files[i].Patch, trimmed = "", true
			continue
		}
		used += size
	}
	return files, trimmed
}

// PullTree loads the repository tree at the head commit of a pull request.
func (s *Service) PullTree(ctx context.Context, token string, repo model.Repo, number int) (TreeResult, error) {
	adapter, err := s.adapter(repo)
	if err != nil {
		return TreeResult{}, err
	}
	detail, _, err := adapter.PullDetail(ctx, token, repo, number)
	if err != nil {
		return TreeResult{}, err
	}
	if detail.HeadSHA == "" {
		return TreeResult{}, fmt.Errorf("service: provider returned no head commit for pull request %d", number)
	}
	return s.Tree(ctx, token, repo, detail.HeadSHA)
}

// Tree loads the repository tree at ref.
func (s *Service) Tree(ctx context.Context, token string, repo model.Repo, ref string) (TreeResult, error) {
	adapter, err := s.adapter(repo)
	if err != nil {
		return TreeResult{}, err
	}
	tree, meta, err := adapter.Tree(ctx, token, repo, ref)
	if err != nil {
		return TreeResult{}, err
	}
	return TreeResult{
		Ref: ref, Entries: tree.Entries, Truncated: tree.Truncated, Stale: meta.Stale, FetchedAt: s.fetchedAt(meta),
	}, nil
}

// File loads one file at ref and prepares it for JSON: binary files have no content, text is
// cut at Limits.FileBytes on a character boundary.
func (s *Service) File(ctx context.Context, token string, repo model.Repo, ref, path string) (FileResult, error) {
	adapter, err := s.adapter(repo)
	if err != nil {
		return FileResult{}, err
	}
	raw, _, err := adapter.File(ctx, token, repo, ref, path)
	if err != nil {
		return FileResult{}, err
	}
	result := FileResult{Ref: ref, Path: path, Size: int64(len(raw))}
	if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) {
		result.Binary = true
		return result, nil
	}
	if s.limits.FileBytes > 0 && len(raw) > s.limits.FileBytes {
		raw, result.Truncated = cutAtRune(raw, s.limits.FileBytes), true
	}
	result.Content = string(raw)
	return result, nil
}

// cutAtRune returns at most n bytes of text without splitting a UTF-8 character.
func cutAtRune(text []byte, n int) []byte {
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

func (s *Service) adapter(repo model.Repo) (provider.Provider, error) {
	adapter := s.providers[repo.Provider]
	if adapter == nil {
		return nil, fmt.Errorf("service: no adapter for provider %q", repo.Provider)
	}
	return adapter, nil
}
