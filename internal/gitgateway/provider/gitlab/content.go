package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/pkg/gitprovider/client"
)

const restPageSize = 100

type diffNode struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
}

// PullFiles implements provider.Provider. It reads the merge request diffs endpoint, whose
// patches are empty for files GitLab considers too large.
func (p *Provider) PullFiles(ctx context.Context, token string, repo model.Repo, number int) (provider.ChangedFiles, provider.Meta, error) {
	nodes, truncated, meta, err := provider.Pages[diffNode](ctx, p.cfg.Client, restPageSize, p.cfg.FilesMaxPages,
		func(page int) client.Request {
			return client.Request{
				URL:   fmt.Sprintf("%s/merge_requests/%d/diffs?per_page=%d&page=%d", projectURL(repo), number, restPageSize, page),
				Token: token, TTL: p.cfg.DetailTTL, Cost: p.cfg.ExpectedCost,
			}
		})
	if err != nil {
		return provider.ChangedFiles{}, provider.Meta{}, wrapRepo(repo, err)
	}
	files := make([]model.ChangedFile, len(nodes))
	for i, n := range nodes {
		files[i] = n.toChangedFile()
	}
	return provider.ChangedFiles{Files: files, Truncated: truncated}, meta, nil
}

func (n diffNode) toChangedFile() model.ChangedFile {
	file := model.ChangedFile{Path: n.NewPath, Status: "modified", Patch: n.Diff}
	switch {
	case n.NewFile:
		file.Status = "added"
	case n.DeletedFile:
		file.Status, file.Path = "removed", n.OldPath
	case n.RenamedFile:
		file.Status, file.PreviousPath = "renamed", n.OldPath
	}
	file.Additions, file.Deletions = countChanges(n.Diff)
	return file
}

// countChanges counts the added and removed lines of a GitLab diff, which has hunk
// headers but no file headers.
func countChanges(diff string) (additions, deletions int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

// Tree implements provider.Provider. GitLab does not report file sizes in trees.
func (p *Provider) Tree(ctx context.Context, token string, repo model.Repo, ref string) (provider.Tree, provider.Meta, error) {
	type node struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	nodes, truncated, meta, err := provider.Pages[node](ctx, p.cfg.Client, restPageSize, p.cfg.FilesMaxPages,
		func(page int) client.Request {
			return client.Request{
				URL: fmt.Sprintf("%s/repository/tree?recursive=true&ref=%s&per_page=%d&page=%d",
					projectURL(repo), url.QueryEscape(ref), restPageSize, page),
				Token: token, TTL: p.cfg.TreeTTL, Cost: p.cfg.ExpectedCost,
			}
		})
	if err != nil {
		return provider.Tree{}, provider.Meta{}, wrapRepo(repo, err)
	}
	tree := provider.Tree{Entries: make([]model.TreeEntry, 0, len(nodes)), Truncated: truncated}
	for _, n := range nodes {
		if n.Type == "blob" || n.Type == "tree" {
			tree.Entries = append(tree.Entries, model.TreeEntry{Path: n.Path, Type: n.Type})
		}
	}
	return tree, meta, nil
}

// File implements provider.Provider.
func (p *Provider) File(ctx context.Context, token string, repo model.Repo, ref, path string) ([]byte, provider.Meta, error) {
	resp, meta, err := provider.Get(ctx, p.cfg.Client, client.Request{
		URL:   fmt.Sprintf("%s/repository/files/%s/raw?ref=%s", projectURL(repo), url.PathEscape(path), url.QueryEscape(ref)),
		Token: token, TTL: p.cfg.FileTTL, Cost: p.cfg.ExpectedCost,
	})
	if err != nil {
		return nil, provider.Meta{}, wrapRepo(repo, err)
	}
	return resp.Body, meta, nil
}

// Archive implements provider.Provider.
func (p *Provider) Archive(ctx context.Context, token string, repo model.Repo, ref string) (provider.ArchiveStream, error) {
	stream, err := provider.GetStream(ctx, p.cfg.Client, client.Request{
		URL:   fmt.Sprintf("%s/repository/archive.tar.gz?sha=%s", projectURL(repo), url.QueryEscape(ref)),
		Token: token, Cost: p.cfg.ExpectedCost,
	})
	if err != nil {
		return provider.ArchiveStream{}, wrapRepo(repo, err)
	}
	return stream, nil
}

// projectURL is the REST URL of the project; the full path is one escaped segment.
func projectURL(repo model.Repo) string {
	return fmt.Sprintf("%s/api/v4/projects/%s", strings.TrimRight(repo.BaseURL, "/"), url.PathEscape(repo.FullPath()))
}

func wrapRepo(repo model.Repo, err error) error {
	return fmt.Errorf("gitlab: project %s: %w", repo.FullPath(), err)
}
