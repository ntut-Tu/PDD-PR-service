// Package gitlab reads merge requests through the GitLab GraphQL API of the
// host each repository lives on.
package gitlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/upstream"
)

// maxProjectsPerQuery is the connection size of the projects field in the ADR query.
const maxProjectsPerQuery = 10

// Config configures the adapter.
type Config struct {
	Client *upstream.Client
	// BatchSize is the number of projects per list query; it is capped at 10.
	BatchSize int
	// MaxPages caps the pages followed for one project.
	MaxPages  int
	ListTTL   time.Duration
	DetailTTL time.Duration
	// TreeTTL and FileTTL are the fresh lifetimes of repository trees and file contents.
	TreeTTL time.Duration
	FileTTL time.Duration
	// FilesMaxPages caps the pages of 100 followed for the files of a pull request or a tree.
	FilesMaxPages int
	ExpectedCost  int
}

// Provider reads data from GitLab.
type Provider struct {
	cfg Config
}

// New creates the adapter.
func New(cfg Config) *Provider {
	cfg.BatchSize = min(cfg.BatchSize, maxProjectsPerQuery)
	return &Provider{cfg: cfg}
}

// Endpoint returns the GraphQL URL of the GitLab host at baseURL.
func Endpoint(baseURL string) string { return strings.TrimRight(baseURL, "/") + "/api/graphql" }

const mergeRequestFields = `iid title draft detailedMergeStatus webUrl updatedAt
          sourceBranch targetBranch diffHeadSha
          author { username avatarUrl }`

type userNode struct {
	Username  string `json:"username"`
	AvatarURL string `json:"avatarUrl"`
}

type mergeRequestNode struct {
	IID                 string    `json:"iid"`
	Title               string    `json:"title"`
	Draft               bool      `json:"draft"`
	DetailedMergeStatus string    `json:"detailedMergeStatus"`
	WebURL              string    `json:"webUrl"`
	UpdatedAt           time.Time `json:"updatedAt"`
	SourceBranch        string    `json:"sourceBranch"`
	TargetBranch        string    `json:"targetBranch"`
	DiffHeadSha         string    `json:"diffHeadSha"`
	Author              *userNode `json:"author"`
}

type mergeRequestPage struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []mergeRequestNode `json:"nodes"`
}

type projectNode struct {
	FullPath      string           `json:"fullPath"`
	MergeRequests mergeRequestPage `json:"mergeRequests"`
}

const listQuery = `query MergeRequestList($paths: [String!]!) {
  projects(fullPaths: $paths, first: 10) {
    nodes {
      fullPath
      mergeRequests(state: opened, first: 50, sort: UPDATED_DESC) {
        pageInfo { hasNextPage endCursor }
        nodes {
          ` + mergeRequestFields + `
        }
      }
    }
  }
}`

const listPageQuery = `query MergeRequestListMore($path: ID!, $after: String!) {
  project(fullPath: $path) {
    mergeRequests(state: opened, first: 50, after: $after, sort: UPDATED_DESC) {
      pageInfo { hasNextPage endCursor }
      nodes {
        ` + mergeRequestFields + `
      }
    }
  }
}`

// ListPulls reads open pull requests. Repositories are grouped by host.
func (p *Provider) ListPulls(ctx context.Context, token string, repos []model.Repo) []provider.RepoPulls {
	results := make([]provider.RepoPulls, len(repos))
	byHost := map[string][]int{}
	var hosts []string
	for i, repo := range repos {
		if _, seen := byHost[repo.BaseURL]; !seen {
			hosts = append(hosts, repo.BaseURL)
		}
		byHost[repo.BaseURL] = append(byHost[repo.BaseURL], i)
	}
	for _, host := range hosts {
		indexes := byHost[host]
		for start := 0; start < len(indexes); start += p.cfg.BatchSize {
			chunk := indexes[start:min(start+p.cfg.BatchSize, len(indexes))]
			batch := make([]model.Repo, len(chunk))
			for j, idx := range chunk {
				batch[j] = repos[idx]
			}
			for j, res := range p.listBatch(ctx, token, host, batch) {
				results[chunk[j]] = res
			}
		}
	}
	return results
}

func (p *Provider) listBatch(ctx context.Context, token, host string, repos []model.Repo) []provider.RepoPulls {
	paths := make([]string, len(repos))
	for i, repo := range repos {
		paths[i] = repo.FullPath()
	}
	data, _, meta, err := provider.Query[struct {
		Projects struct {
			Nodes []projectNode `json:"nodes"`
		} `json:"projects"`
	}](ctx, p.cfg.Client, upstream.GraphQLRequest{
		Endpoint:  Endpoint(host),
		Token:     token,
		Query:     listQuery,
		Variables: map[string]any{"paths": paths},
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
		node := findProject(data.Projects.Nodes, repo.FullPath())
		if node == nil {
			results[i].Err = fmt.Errorf("gitlab: project %s: %w", repo.FullPath(), provider.ErrNotFound)
			continue
		}
		p.collect(ctx, token, &results[i], node.MergeRequests)
	}
	return results
}

func findProject(nodes []projectNode, fullPath string) *projectNode {
	for i := range nodes {
		if strings.EqualFold(nodes[i].FullPath, fullPath) {
			return &nodes[i]
		}
	}
	return nil
}

// collect maps the first page and follows further pages up to MaxPages.
func (p *Provider) collect(ctx context.Context, token string, out *provider.RepoPulls, page mergeRequestPage) {
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

func (p *Provider) listPage(ctx context.Context, token string, repo model.Repo, after string) (mergeRequestPage, provider.Meta, error) {
	data, _, meta, err := provider.Query[struct {
		Project *struct {
			MergeRequests mergeRequestPage `json:"mergeRequests"`
		} `json:"project"`
	}](ctx, p.cfg.Client, upstream.GraphQLRequest{
		Endpoint:  Endpoint(repo.BaseURL),
		Token:     token,
		Query:     listPageQuery,
		Variables: map[string]any{"path": repo.FullPath(), "after": after},
		TTL:       p.cfg.ListTTL,
		Cost:      p.cfg.ExpectedCost,
	})
	if err != nil {
		return mergeRequestPage{}, provider.Meta{}, err
	}
	if data.Project == nil {
		return mergeRequestPage{}, provider.Meta{}, fmt.Errorf("gitlab: project %s: %w", repo.FullPath(), provider.ErrNotFound)
	}
	return data.Project.MergeRequests, meta, nil
}

func (n mergeRequestNode) toPull(repo model.Repo) model.Pull {
	number, _ := strconv.Atoi(n.IID)
	return model.Pull{
		RepoID:       repo.ID,
		Provider:     model.GitLab,
		Repo:         model.RepoRef{Owner: repo.Owner, Name: repo.Name, URL: repo.URL},
		Number:       number,
		Title:        n.Title,
		State:        "open",
		Draft:        n.Draft,
		Mergeable:    mapMergeable(n.DetailedMergeStatus),
		Author:       n.Author.toUser(repo.BaseURL),
		SourceBranch: n.SourceBranch,
		TargetBranch: n.TargetBranch,
		HeadSHA:      n.DiffHeadSha,
		URL:          n.WebURL,
		UpdatedAt:    n.UpdatedAt,
	}
}

func (u *userNode) toUser(baseURL string) model.User {
	if u == nil {
		return model.User{}
	}
	return model.User{Login: u.Username, AvatarURL: provider.AbsoluteURL(baseURL, u.AvatarURL)}
}

func mapMergeable(status string) model.Mergeable {
	switch status {
	case "MERGEABLE":
		return model.MergeableYes
	case "CONFLICTS":
		return model.MergeableConflict
	default:
		return model.MergeableUnknown
	}
}

type detailNode struct {
	mergeRequestNode
	Description string `json:"description"`
	DiffRefs    *struct {
		BaseSha string `json:"baseSha"`
	} `json:"diffRefs"`
	Commits struct {
		Nodes []struct {
			SHA           string    `json:"sha"`
			Message       string    `json:"message"`
			AuthorName    string    `json:"authorName"`
			CommittedDate time.Time `json:"committedDate"`
			WebURL        string    `json:"webUrl"`
		} `json:"nodes"`
	} `json:"commits"`
	Notes struct {
		Nodes []struct {
			ID        string    `json:"id"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"createdAt"`
			System    bool      `json:"system"`
			Author    *userNode `json:"author"`
		} `json:"nodes"`
	} `json:"notes"`
}

const detailQuery = `query MergeRequestDetail($path: ID!, $iid: String!) {
  project(fullPath: $path) {
    mergeRequest(iid: $iid) {
      ` + mergeRequestFields + ` description
      diffRefs { baseSha }
      commits(first: 100) { nodes { sha title message authorName committedDate webUrl } }
      notes(first: 100) {
        nodes { id body createdAt system author { username avatarUrl } }
      }
    }
  }
}`

// PullDetail reads a pull request with commits and comments. System notes are dropped.
func (p *Provider) PullDetail(ctx context.Context, token string, repo model.Repo, number int) (model.PullDetail, provider.Meta, error) {
	data, _, meta, err := provider.Query[struct {
		Project *struct {
			MergeRequest *detailNode `json:"mergeRequest"`
		} `json:"project"`
	}](ctx, p.cfg.Client, upstream.GraphQLRequest{
		Endpoint:  Endpoint(repo.BaseURL),
		Token:     token,
		Query:     detailQuery,
		Variables: map[string]any{"path": repo.FullPath(), "iid": strconv.Itoa(number)},
		TTL:       p.cfg.DetailTTL,
		Cost:      p.cfg.ExpectedCost,
	})
	if err != nil {
		return model.PullDetail{}, provider.Meta{}, err
	}
	if data.Project == nil || data.Project.MergeRequest == nil {
		return model.PullDetail{}, provider.Meta{}, fmt.Errorf("gitlab: merge request %s!%d: %w", repo.FullPath(), number, provider.ErrNotFound)
	}
	return data.Project.MergeRequest.toDetail(repo), meta, nil
}

func (n *detailNode) toDetail(repo model.Repo) model.PullDetail {
	detail := model.PullDetail{
		Pull:        n.toPull(repo),
		Description: n.Description,
		Commits:     make([]model.Commit, 0, len(n.Commits.Nodes)),
		Comments:    make([]model.Comment, 0, len(n.Notes.Nodes)),
	}
	if n.DiffRefs != nil {
		detail.BaseSHA = n.DiffRefs.BaseSha
	}
	for _, c := range n.Commits.Nodes {
		detail.Commits = append(detail.Commits, model.Commit{
			SHA: c.SHA, Message: c.Message, Author: c.AuthorName, CommittedAt: c.CommittedDate, URL: c.WebURL,
		})
	}
	for _, note := range n.Notes.Nodes {
		if note.System {
			continue
		}
		detail.Comments = append(detail.Comments, model.Comment{
			ID: note.ID, Author: note.Author.toUser(repo.BaseURL), Body: note.Body, CreatedAt: note.CreatedAt,
		})
	}
	return detail
}
