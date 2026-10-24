// Package model holds the types shared by the providers, the service and the
// HTTP API. The JSON form is the public API of the gateway.
package model

import "time"

// Provider names a Git hosting provider.
type Provider string

// Supported providers.
const (
	GitHub Provider = "github"
	GitLab Provider = "gitlab"
)

// Mergeable is the merge state of a pull request.
type Mergeable string

// Merge states.
const (
	MergeableYes      Mergeable = "mergeable"
	MergeableConflict Mergeable = "conflicting"
	MergeableUnknown  Mergeable = "unknown"
)

// Repo is a repository bound to a PDD project.
type Repo struct {
	ID       int64
	Provider Provider
	// BaseURL is scheme and host of the repository URL, used for self-hosted GitLab.
	BaseURL string
	// Owner is the user, organization or nested GitLab group path.
	Owner string
	Name  string
	URL   string
}

// FullPath returns owner/name.
func (r Repo) FullPath() string { return r.Owner + "/" + r.Name }

// RepoRef is the repository part of a pull request.
type RepoRef struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

// User is the author of a pull request, commit or comment.
type User struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatarUrl"`
}

// Pull is the summary of an open pull request or merge request.
type Pull struct {
	RepoID       int64     `json:"repoId"`
	Provider     Provider  `json:"provider"`
	Repo         RepoRef   `json:"repo"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	Draft        bool      `json:"draft"`
	Mergeable    Mergeable `json:"mergeable"`
	Author       User      `json:"author"`
	SourceBranch string    `json:"sourceBranch"`
	TargetBranch string    `json:"targetBranch"`
	// HeadSHA is the commit at the tip of the source branch.
	HeadSHA   string    `json:"headSha"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Commit is one commit of a pull request.
type Commit struct {
	SHA         string    `json:"sha"`
	Message     string    `json:"message"`
	Author      string    `json:"author"`
	CommittedAt time.Time `json:"committedAt"`
	URL         string    `json:"url"`
}

// Comment is one discussion comment of a pull request.
type Comment struct {
	ID        string    `json:"id"`
	Author    User      `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// ChangedFile is one file changed by a pull request.
type ChangedFile struct {
	Path string `json:"path"`
	// PreviousPath is the old path of a renamed file, empty otherwise.
	PreviousPath string `json:"previousPath"`
	// Status is added, modified, removed or renamed.
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	// Patch is the unified diff; empty for binary or oversized files.
	Patch string `json:"patch"`
}

// TreeEntry is one entry of a repository tree.
type TreeEntry struct {
	Path string `json:"path"`
	// Type is blob or tree.
	Type string `json:"type"`
	// Size is the file size in bytes; zero for trees and when the provider does not report it.
	Size int64 `json:"size"`
}

// PullDetail is a pull request with its description, commits and comments.
type PullDetail struct {
	Pull
	// BaseSHA is the commit of the target branch the pull request is based on.
	BaseSHA     string    `json:"baseSha"`
	Description string    `json:"description"`
	Commits     []Commit  `json:"commits"`
	Comments    []Comment `json:"comments"`
}
