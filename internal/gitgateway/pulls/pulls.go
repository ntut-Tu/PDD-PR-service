// Package pulls manages project pull request reads, partial failures and patch limits.
package pulls

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/repos"
	"pdd/services/internal/gitgateway/upstream"
)

// Error codes reported to API clients.
const (
	CodeUpstreamFailed       = "upstream_failed"
	CodeQuotaReserve         = "quota_reserve"
	CodeProviderUnauthorized = "provider_unauthorized"
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

// Provider is the pull request capability needed by this component.
type Provider interface {
	ListPulls(ctx context.Context, token string, repos []model.Repo) []provider.RepoPulls
	PullDetail(ctx context.Context, token string, repo model.Repo, number int) (model.PullDetail, provider.Meta, error)
	PullFiles(ctx context.Context, token string, repo model.Repo, number int) (provider.ChangedFiles, provider.Meta, error)
}

// Pulls combines project pull requests and coordinates reads tied to a pull's head commit.
type Pulls struct {
	providers     map[model.Provider]Provider
	repos         *repos.Repos
	now           func() time.Time
	maxPatchBytes int64
}

// New creates the pull request component. A zero patch limit means unlimited.
func New(providers map[model.Provider]Provider, content *repos.Repos, now func() time.Time, maxPatchBytes int64) *Pulls {
	if now == nil {
		now = time.Now
	}
	if content == nil {
		content = repos.New(nil, now, 0)
	}
	return &Pulls{providers: providers, repos: content, now: now, maxPatchBytes: maxPatchBytes}
}

// ListPulls lists the open pull requests of repos, newest update first. A
// repository that fails does not fail the list: it is reported in Errors.
func (s *Pulls) ListPulls(ctx context.Context, token string, repos []model.Repo) ListResult {
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
func (s *Pulls) PullDetail(ctx context.Context, token string, repo model.Repo, number int) (DetailResult, error) {
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
	case errors.Is(err, upstream.ErrQuotaReserve):
		return CodeQuotaReserve
	case errors.Is(err, upstream.ErrUnauthorized):
		return CodeProviderUnauthorized
	default:
		return CodeUpstreamFailed
	}
}

func (s *Pulls) fetchedAt(meta provider.Meta) time.Time {
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
		cause = cmp.Or(res.Meta.StaleReason, upstream.ErrMaxRetry)
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
