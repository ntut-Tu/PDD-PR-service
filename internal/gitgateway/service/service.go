// Package service combines the repositories of a project, the provider adapters
// and the error policy of the gateway API.
package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"golang.org/x/sync/singleflight"

	"pdd/services/internal/gitgateway/archive"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/pkg/gitprovider"
)

// Error codes reported to API clients.
const (
	CodeUpstreamFailed       = "upstream_failed"
	CodeQuotaReserve         = "quota_reserve"
	CodeProviderUnauthorized = "provider_unauthorized"
	CodeArchiveTooLarge      = "archive_too_large"
)

// RepoError reports why one repository could not be read or is served stale.
type RepoError struct {
	RepoID  int64  `json:"repoId"`
	Repo    string `json:"repo"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ListResult is the open pull requests of a project.
type ListResult struct {
	Pulls []model.Pull
	// Stale is true when any repository was served from stale cache.
	Stale bool
	// FetchedAt is the oldest time the provider confirmed the data.
	FetchedAt time.Time
	Errors    []RepoError
}

// DetailResult is one pull request with commits and comments.
type DetailResult struct {
	Pull      model.PullDetail
	Stale     bool
	FetchedAt time.Time
}

// Service reads pull requests through the provider adapters.
type Service struct {
	providers map[model.Provider]provider.Provider
	now       func() time.Time
	limits    Limits
	archives  *archive.Cache
	flight    singleflight.Group
}

// Limits bound the size of the content returned to API clients. Zero means unlimited.
type Limits struct {
	// PatchBytes is the total size of the patches of one pull request.
	PatchBytes int64
	// FileBytes is the largest file content.
	FileBytes int
	// ArchiveBytes is the largest source archive.
	ArchiveBytes int64
}

// Option configures optional parts of a Service.
type Option func(*Service)

// WithArchiveCache enables source archives, which are stored in cache.
func WithArchiveCache(cache *archive.Cache) Option {
	return func(s *Service) { s.archives = cache }
}

// New creates a Service. A nil now defaults to time.Now.
func New(providers map[model.Provider]provider.Provider, now func() time.Time, limits Limits, opts ...Option) *Service {
	if now == nil {
		now = time.Now
	}
	s := &Service{providers: providers, now: now, limits: limits}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ListPulls lists the open pull requests of repos, newest update first. A
// repository that fails does not fail the list: it is reported in Errors.
func (s *Service) ListPulls(ctx context.Context, token string, repos []model.Repo) ListResult {
	result := ListResult{Pulls: []model.Pull{}, Errors: []RepoError{}}
	var meta provider.Meta
	for _, kind := range []model.Provider{model.GitHub, model.GitLab} {
		adapter, group := s.providers[kind], reposOf(repos, kind)
		if adapter == nil || len(group) == 0 {
			continue
		}
		for _, res := range adapter.ListPulls(ctx, token, group) {
			meta = meta.Merge(res.Meta)
			result.Pulls = append(result.Pulls, res.Pulls...)
			if re, ok := repoError(res); ok {
				result.Errors = append(result.Errors, re)
			}
		}
	}
	sort.SliceStable(result.Pulls, func(i, j int) bool { return result.Pulls[i].UpdatedAt.After(result.Pulls[j].UpdatedAt) })
	result.Stale = meta.Stale
	result.FetchedAt = s.fetchedAt(meta)
	return result
}

// PullDetail loads one pull request. The error is the provider error as is;
// use Code to classify it.
func (s *Service) PullDetail(ctx context.Context, token string, repo model.Repo, number int) (DetailResult, error) {
	adapter := s.providers[repo.Provider]
	if adapter == nil {
		return DetailResult{}, fmt.Errorf("service: no adapter for provider %q", repo.Provider)
	}
	detail, meta, err := adapter.PullDetail(ctx, token, repo, number)
	if err != nil {
		return DetailResult{}, err
	}
	return DetailResult{Pull: detail, Stale: meta.Stale, FetchedAt: s.fetchedAt(meta)}, nil
}

// Code maps a provider error to an API error code.
func Code(err error) string {
	switch {
	case errors.Is(err, gitprovider.ErrQuotaReserve):
		return CodeQuotaReserve
	case errors.Is(err, gitprovider.ErrUnauthorized):
		return CodeProviderUnauthorized
	default:
		return CodeUpstreamFailed
	}
}

func (s *Service) fetchedAt(meta provider.Meta) time.Time {
	if meta.FetchedAt.IsZero() {
		return s.now().UTC()
	}
	return meta.FetchedAt.UTC()
}

func reposOf(repos []model.Repo, kind model.Provider) []model.Repo {
	var group []model.Repo
	for _, repo := range repos {
		if repo.Provider == kind {
			group = append(group, repo)
		}
	}
	return group
}

// repoError describes a failed repository, or a repository served from stale cache.
// Messages are user-facing; the underlying cause only selects the code.
func repoError(res provider.RepoPulls) (RepoError, bool) {
	cause := res.Err
	message := ""
	switch {
	case cause != nil:
		message = userMessage(Code(cause))
	case res.Meta.Stale:
		cause = cmp.Or(res.Meta.StaleReason, gitprovider.ErrMaxRetry)
		message = "Showing cached data. " + userMessage(Code(cause))
	default:
		return RepoError{}, false
	}
	return RepoError{RepoID: res.Repo.ID, Repo: res.Repo.FullPath(), Code: Code(cause), Message: message}, true
}

func userMessage(code string) string {
	switch code {
	case CodeQuotaReserve:
		return "The API quota of your account is almost used up, so the provider was not asked again."
	case CodeProviderUnauthorized:
		return "The provider rejected your access token. Sign in with this provider to see this repository."
	default:
		return "The provider could not be reached. Try again later."
	}
}
