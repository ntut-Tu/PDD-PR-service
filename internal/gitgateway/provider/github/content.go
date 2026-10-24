package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/pkg/gitprovider/client"
)

const restPageSize = 100

type fileNode struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

// PullFiles implements provider.Provider.
func (p *Provider) PullFiles(ctx context.Context, token string, repo model.Repo, number int) (provider.ChangedFiles, provider.Meta, error) {
	nodes, truncated, meta, err := provider.Pages[fileNode](ctx, p.cfg.Client, restPageSize, p.cfg.FilesMaxPages,
		func(page int) client.Request {
			return client.Request{
				URL:   fmt.Sprintf("%s/pulls/%d/files?per_page=%d&page=%d", p.repoURL(repo), number, restPageSize, page),
				Token: token, TTL: p.cfg.DetailTTL, Cost: p.cfg.ExpectedCost,
			}
		})
	if err != nil {
		return provider.ChangedFiles{}, provider.Meta{}, wrapRepo(repo, err)
	}
	files := make([]model.ChangedFile, len(nodes))
	for i, n := range nodes {
		files[i] = model.ChangedFile{
			Path: n.Filename, PreviousPath: n.PreviousFilename, Status: mapFileStatus(n.Status),
			Additions: n.Additions, Deletions: n.Deletions, Patch: n.Patch,
		}
	}
	return provider.ChangedFiles{Files: files, Truncated: truncated}, meta, nil
}

// mapFileStatus reduces the GitHub file statuses to added, modified, removed and renamed.
func mapFileStatus(status string) string {
	switch status {
	case "added", "copied":
		return "added"
	case "removed":
		return "removed"
	case "renamed":
		return "renamed"
	default:
		return "modified"
	}
}

// Tree implements provider.Provider.
func (p *Provider) Tree(ctx context.Context, token string, repo model.Repo, ref string) (provider.Tree, provider.Meta, error) {
	resp, meta, err := provider.Get(ctx, p.cfg.Client, client.Request{
		URL:   fmt.Sprintf("%s/git/trees/%s?recursive=1", p.repoURL(repo), url.PathEscape(ref)),
		Token: token, TTL: p.cfg.TreeTTL, Cost: p.cfg.ExpectedCost,
	})
	if err != nil {
		return provider.Tree{}, provider.Meta{}, wrapRepo(repo, err)
	}
	var body struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := provider.Decode(resp.Body, &body); err != nil {
		return provider.Tree{}, provider.Meta{}, wrapRepo(repo, err)
	}
	tree := provider.Tree{Entries: make([]model.TreeEntry, 0, len(body.Tree)), Truncated: body.Truncated}
	for _, e := range body.Tree {
		if e.Type == "blob" || e.Type == "tree" {
			tree.Entries = append(tree.Entries, model.TreeEntry{Path: e.Path, Type: e.Type, Size: e.Size})
		}
	}
	return tree, meta, nil
}

// File implements provider.Provider. The raw media type returns the bytes of the file itself,
// for files of any size the API allows.
func (p *Provider) File(ctx context.Context, token string, repo model.Repo, ref, path string) ([]byte, provider.Meta, error) {
	resp, meta, err := provider.Get(ctx, p.cfg.Client, client.Request{
		URL:    fmt.Sprintf("%s/contents/%s?ref=%s", p.repoURL(repo), escapePath(path), url.QueryEscape(ref)),
		Token:  token,
		Header: http.Header{"Accept": {"application/vnd.github.raw+json"}},
		TTL:    p.cfg.FileTTL, Cost: p.cfg.ExpectedCost,
	})
	if err != nil {
		return nil, provider.Meta{}, wrapRepo(repo, err)
	}
	return resp.Body, meta, nil
}

func (p *Provider) repoURL(repo model.Repo) string {
	return fmt.Sprintf("%s/repos/%s/%s", strings.TrimRight(p.cfg.APIURL, "/"), url.PathEscape(repo.Owner), url.PathEscape(repo.Name))
}

// escapePath escapes each segment of a repository path and keeps the slashes.
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func wrapRepo(repo model.Repo, err error) error {
	return fmt.Errorf("github: repository %s: %w", repo.FullPath(), err)
}
