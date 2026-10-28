// Package repos reads repository trees and files and prepares file content for the gateway.
package repos

import (
	"bytes"
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
)

// Provider is the repository content capability needed by this component.
type Provider interface {
	Tree(ctx context.Context, token string, repo model.Repo, ref string) (provider.Tree, provider.Meta, error)
	File(ctx context.Context, token string, repo model.Repo, ref, path string) ([]byte, provider.Meta, error)
}

// Repos routes content reads and applies the file byte limit and binary detection.
type Repos struct {
	providers    map[model.Provider]Provider
	now          func() time.Time
	maxFileBytes int
}

// New creates the content component. A zero byte limit means unlimited.
func New(providers map[model.Provider]Provider, now func() time.Time, maxFileBytes int) *Repos {
	if now == nil {
		now = time.Now
	}
	return &Repos{providers: providers, now: now, maxFileBytes: maxFileBytes}
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
	// Content is UTF-8 text, cut at the configured byte limit; empty for binary files.
	Content   string
	Binary    bool
	Truncated bool
}

// Tree loads the repository tree at ref.
func (s *Repos) Tree(ctx context.Context, token string, repo model.Repo, ref string) (TreeResult, error) {
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
// cut at the configured byte limit on a character boundary.
func (s *Repos) File(ctx context.Context, token string, repo model.Repo, ref, path string) (FileResult, error) {
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
	if s.maxFileBytes > 0 && len(raw) > s.maxFileBytes {
		raw, result.Truncated = cutAtRune(raw, s.maxFileBytes), true
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

func (s *Repos) adapter(repo model.Repo) (Provider, error) {
	adapter := s.providers[repo.Provider]
	if adapter == nil {
		return nil, fmt.Errorf("service: no adapter for provider %q", repo.Provider)
	}
	return adapter, nil
}

func (s *Repos) fetchedAt(meta provider.Meta) time.Time {
	if meta.FetchedAt.IsZero() {
		return s.now().UTC()
	}
	return meta.FetchedAt.UTC()
}
