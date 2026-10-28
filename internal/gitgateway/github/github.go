// Package github reads pull requests through the GitHub GraphQL API.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/upstream"
)

const pageSize = 50

// Config configures the adapter.
type Config struct {
	// APIURL is the REST base URL (https://api.github.com or https://host/api/v3
	// for GitHub Enterprise Server); the GraphQL endpoint is derived from it.
	APIURL string
	Client *upstream.Client
	// BatchSize is the number of repositories per list query.
	BatchSize int
	// MaxPages caps the pages followed for one repository.
	MaxPages  int
	ListTTL   time.Duration
	DetailTTL time.Duration
	// TreeTTL and FileTTL are the fresh lifetimes of repository trees and file contents.
	TreeTTL time.Duration
	FileTTL time.Duration
	// FilesMaxPages caps the pages of 100 followed for the files of a pull request.
	FilesMaxPages int
	ExpectedCost  int
}

// Provider reads data from GitHub.
type Provider struct {
	cfg      Config
	endpoint string
}

// New creates the adapter.
func New(cfg Config) *Provider {
	return &Provider{cfg: cfg, endpoint: GraphQLEndpoint(cfg.APIURL)}
}

// GraphQLEndpoint derives the GraphQL URL from the REST API base URL.
func GraphQLEndpoint(apiURL string) string {
	base := strings.TrimRight(apiURL, "/")
	if prefix, ok := strings.CutSuffix(base, "/api/v3"); ok {
		return prefix + "/api/graphql"
	}
	return base + "/graphql"
}

const rateLimitSelection = "rateLimit { cost remaining resetAt limit }"

const pullFields = `number title state isDraft mergeable url updatedAt
      headRefName baseRefName headRefOid
      author { login avatarUrl }`

type pullNode struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	IsDraft     bool      `json:"isDraft"`
	Mergeable   string    `json:"mergeable"`
	URL         string    `json:"url"`
	UpdatedAt   time.Time `json:"updatedAt"`
	HeadRefName string    `json:"headRefName"`
	BaseRefName string    `json:"baseRefName"`
	HeadRefOid  string    `json:"headRefOid"`
	Author      *userNode `json:"author"`
}

type userNode struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl"`
}

type pullPage struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []pullNode `json:"nodes"`
}

type repoPulls struct {
	PullRequests pullPage `json:"pullRequests"`
}

// ListPulls reads open pull requests.
func (p *Provider) ListPulls(ctx context.Context, token string, repos []model.Repo) []provider.RepoPulls {
	results := make([]provider.RepoPulls, 0, len(repos))
	for start := 0; start < len(repos); start += p.cfg.BatchSize {
		end := min(start+p.cfg.BatchSize, len(repos))
		results = append(results, p.listBatch(ctx, token, repos[start:end])...)
	}
	return results
}

func (p *Provider) listBatch(ctx context.Context, token string, repos []model.Repo) []provider.RepoPulls {
	data, errs, meta, err := provider.Query[map[string]json.RawMessage](ctx, p.cfg.Client, upstream.GraphQLRequest{
		Endpoint:  p.endpoint,
		Token:     token,
		Query:     listQuery(len(repos)),
		Variables: listVariables(repos),
		TTL:       p.cfg.ListTTL,
		Cost:      p.cfg.ExpectedCost,
	})
	results := make([]provider.RepoPulls, len(repos))
	for i, repo := range repos {
		results[i] = provider.RepoPulls{Repo: repo, Meta: meta}
		if err != nil {
			results[i].Err = err
			continue
		}
		var node *repoPulls
		if raw := (*data)[alias(i)]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &node); err != nil {
				results[i].Err = fmt.Errorf("github: decode %s: %w", repo.FullPath(), err)
				continue
			}
		}
		if node == nil {
			results[i].Err = notFound(repo, errs)
			continue
		}
		p.collect(ctx, token, &results[i], node.PullRequests)
	}
	return results
}

// collect maps the first page and follows further pages up to MaxPages.
func (p *Provider) collect(ctx context.Context, token string, out *provider.RepoPulls, page pullPage) {
	for n := 1; ; n++ {
		for _, node := range page.Nodes {
			out.Pulls = append(out.Pulls, node.toPull(out.Repo))
		}
		if !page.PageInfo.HasNextPage || n >= p.cfg.MaxPages {
			return
		}
		next, meta, err := p.listPage(ctx, token, out.Repo, page.PageInfo.EndCursor)
		if err != nil {
			out.Pulls, out.Err = nil, err
			return
		}
		out.Meta = out.Meta.Merge(meta)
		page = next
	}
}

func (p *Provider) listPage(ctx context.Context, token string, repo model.Repo, after string) (pullPage, provider.Meta, error) {
	data, errs, meta, err := provider.Query[struct {
		Repository *repoPulls `json:"repository"`
	}](ctx, p.cfg.Client, upstream.GraphQLRequest{
		Endpoint:  p.endpoint,
		Token:     token,
		Query:     listPageQuery,
		Variables: map[string]any{"owner": repo.Owner, "name": repo.Name, "after": after},
		TTL:       p.cfg.ListTTL,
		Cost:      p.cfg.ExpectedCost,
	})
	if err != nil {
		return pullPage{}, provider.Meta{}, err
	}
	if data.Repository == nil {
		return pullPage{}, provider.Meta{}, notFound(repo, errs)
	}
	return data.Repository.PullRequests, meta, nil
}

func notFound(repo model.Repo, errs provider.GraphQLErrors) error {
	if len(errs) > 0 {
		return fmt.Errorf("github: repository %s: %w", repo.FullPath(), errs)
	}
	return fmt.Errorf("github: repository %s: %w", repo.FullPath(), provider.ErrNotFound)
}

func alias(i int) string { return fmt.Sprintf("r%d", i) }

func listVariables(repos []model.Repo) map[string]any {
	vars := make(map[string]any, 2*len(repos))
	for i, repo := range repos {
		vars[fmt.Sprintf("owner%d", i)] = repo.Owner
		vars[fmt.Sprintf("name%d", i)] = repo.Name
	}
	return vars
}

// listQuery builds the batched list query of the ADR for n repositories.
func listQuery(n int) string {
	var decls, selections []string
	for i := 0; i < n; i++ {
		decls = append(decls, fmt.Sprintf("$owner%[1]d: String!, $name%[1]d: String!", i))
		selections = append(selections, fmt.Sprintf("%s: repository(owner: $owner%d, name: $name%d) { ...PullPage }", alias(i), i, i))
	}
	return fmt.Sprintf(`query PullList(%s) {
  %s
  %s
}

fragment PullPage on Repository {
  pullRequests(states: OPEN, first: %d, orderBy: {field: UPDATED_AT, direction: DESC}) {
    pageInfo { hasNextPage endCursor }
    nodes {
      %s
    }
  }
}`, strings.Join(decls, ", "), strings.Join(selections, "\n  "), rateLimitSelection, pageSize, pullFields)
}

var listPageQuery = fmt.Sprintf(`query PullListMore($owner: String!, $name: String!, $after: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: %d, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        %s
      }
    }
  }
  %s
}`, pageSize, pullFields, rateLimitSelection)

func (n pullNode) toPull(repo model.Repo) model.Pull {
	return model.Pull{
		RepoID:       repo.ID,
		Provider:     model.GitHub,
		Repo:         model.RepoRef{Owner: repo.Owner, Name: repo.Name, URL: repo.URL},
		Number:       n.Number,
		Title:        n.Title,
		State:        "open",
		Draft:        n.IsDraft,
		Mergeable:    mapMergeable(n.Mergeable),
		Author:       n.Author.toUser(),
		SourceBranch: n.HeadRefName,
		TargetBranch: n.BaseRefName,
		HeadSHA:      n.HeadRefOid,
		URL:          n.URL,
		UpdatedAt:    n.UpdatedAt,
	}
}

func (u *userNode) toUser() model.User {
	if u == nil {
		return model.User{Login: "ghost"}
	}
	return model.User{Login: u.Login, AvatarURL: u.AvatarURL}
}

func mapMergeable(state string) model.Mergeable {
	switch state {
	case "MERGEABLE":
		return model.MergeableYes
	case "CONFLICTING":
		return model.MergeableConflict
	default:
		return model.MergeableUnknown
	}
}

type detailNode struct {
	pullNode
	Body       string `json:"body"`
	BaseRefOid string `json:"baseRefOid"`
	Commits    struct {
		Nodes []struct {
			Commit struct {
				OID       string    `json:"oid"`
				Message   string    `json:"message"`
				Committed time.Time `json:"committedDate"`
				URL       string    `json:"url"`
				Author    *struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Comments struct {
		Nodes []struct {
			ID        string    `json:"id"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"createdAt"`
			Author    *userNode `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

var detailQuery = `query PullDetail($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      ` + pullFields + ` body baseRefOid
      commits(first: 100) {
        totalCount
        nodes { commit { oid messageHeadline message committedDate url author { name } } }
      }
      comments(first: 100) {
        totalCount
        nodes { id body createdAt author { login avatarUrl } }
      }
    }
  }
  ` + rateLimitSelection + `
}`

// PullDetail reads a pull request with commits and comments.
func (p *Provider) PullDetail(ctx context.Context, token string, repo model.Repo, number int) (model.PullDetail, provider.Meta, error) {
	data, errs, meta, err := provider.Query[struct {
		Repository *struct {
			PullRequest *detailNode `json:"pullRequest"`
		} `json:"repository"`
	}](ctx, p.cfg.Client, upstream.GraphQLRequest{
		Endpoint:  p.endpoint,
		Token:     token,
		Query:     detailQuery,
		Variables: map[string]any{"owner": repo.Owner, "name": repo.Name, "number": number},
		TTL:       p.cfg.DetailTTL,
		Cost:      p.cfg.ExpectedCost,
	})
	if err != nil {
		return model.PullDetail{}, provider.Meta{}, err
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		return model.PullDetail{}, provider.Meta{}, notFound(repo, errs)
	}
	return data.Repository.PullRequest.toDetail(repo), meta, nil
}

func (n *detailNode) toDetail(repo model.Repo) model.PullDetail {
	detail := model.PullDetail{
		Pull:        n.toPull(repo),
		BaseSHA:     n.BaseRefOid,
		Description: n.Body,
		Commits:     make([]model.Commit, 0, len(n.Commits.Nodes)),
		Comments:    make([]model.Comment, 0, len(n.Comments.Nodes)),
	}
	for _, c := range n.Commits.Nodes {
		commit := model.Commit{
			SHA: c.Commit.OID, Message: c.Commit.Message, CommittedAt: c.Commit.Committed, URL: c.Commit.URL,
		}
		if c.Commit.Author != nil {
			commit.Author = c.Commit.Author.Name
		}
		detail.Commits = append(detail.Commits, commit)
	}
	for _, c := range n.Comments.Nodes {
		detail.Comments = append(detail.Comments, model.Comment{
			ID: c.ID, Author: c.Author.toUser(), Body: c.Body, CreatedAt: c.CreatedAt,
		})
	}
	return detail
}
