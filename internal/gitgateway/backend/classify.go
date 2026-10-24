package backend

import (
	"net/url"
	"strings"

	"pdd/services/internal/gitgateway/model"
)

// repoEntry is one element of the mixed list returned by GET /repo/{projectId}.
// Jira, Sonarqube and Jenkins entries carry a non-empty type.
type repoEntry struct {
	ID    int64  `json:"id"`
	URL   string `json:"url"`
	Type  string `json:"type"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// baseURL derives the GitLab root of a repository URL: scheme, host, port and the path prefix
// of a server that is not installed at the root. The repository path is /<owner>/<name>, optionally
// followed by .git or a slash, and is cut off; owner may be a nested group path. The gateway cannot
// read the root URL configured in the backend, so this is the only source of the prefix.
func baseURL(u *url.URL, owner, name string) string {
	root := u.Scheme + "://" + u.Host
	path := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
	prefix, found := strings.CutSuffix(path, "/"+strings.Trim(owner, "/")+"/"+name)
	if !found {
		return root
	}
	return root + prefix
}

// classify keeps the GitHub and GitLab repositories of a mixed repo list.
// A github.com URL is GitHub; any other entry without a type that has owner and
// name is a GitLab project on the host (and path prefix) of its URL. Everything else is ignored.
func classify(entries []repoEntry) []model.Repo {
	repos := make([]model.Repo, 0, len(entries))
	for _, e := range entries {
		if repo, ok := classifyEntry(e); ok {
			repos = append(repos, repo)
		}
	}
	return repos
}

func classifyEntry(e repoEntry) (model.Repo, bool) {
	if e.Type != "" || e.Owner == "" || e.Name == "" {
		return model.Repo{}, false
	}
	u, err := url.Parse(e.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return model.Repo{}, false
	}
	repo := model.Repo{
		ID:      e.ID,
		BaseURL: baseURL(u, e.Owner, e.Name),
		Owner:   e.Owner,
		Name:    e.Name,
		URL:     e.URL,
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com":
		repo.Provider = model.GitHub
	default:
		repo.Provider = model.GitLab
	}
	return repo, true
}
