// Package provider defines the contract between the gateway service and the
// GitHub and GitLab adapters, and the helpers the adapters share.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/client"
)

// ErrNotFound is matched by errors.Is when the provider has no such repository
// or pull request, or the token cannot see it.
var ErrNotFound = errors.New("provider: not found")

// Provider reads pull requests of one hosting provider.
type Provider interface {
	// ListPulls returns the open pull requests of every repo, one RepoPulls per
	// repo in the same order. A failure affects only the repos it concerns.
	ListPulls(ctx context.Context, token string, repos []model.Repo) []RepoPulls
	// PullDetail returns one pull request with its commits and comments.
	PullDetail(ctx context.Context, token string, repo model.Repo, number int) (model.PullDetail, Meta, error)
	// PullFiles returns the files changed by a pull request.
	PullFiles(ctx context.Context, token string, repo model.Repo, number int) (ChangedFiles, Meta, error)
	// Tree returns the recursive file tree of repo at ref (a commit SHA).
	Tree(ctx context.Context, token string, repo model.Repo, ref string) (Tree, Meta, error)
	// File returns the raw content of path at ref. The content is not limited here.
	File(ctx context.Context, token string, repo model.Repo, ref, path string) ([]byte, Meta, error)
}

// ChangedFiles is the file list of a pull request.
type ChangedFiles struct {
	Files []model.ChangedFile
	// Truncated is true when the provider has more files than the page cap allows.
	Truncated bool
}

// Tree is a repository tree.
type Tree struct {
	Entries []model.TreeEntry
	// Truncated is true when the provider or the page cap left entries out.
	Truncated bool
}

// RepoPulls is the list result of one repository.
type RepoPulls struct {
	Repo  model.Repo
	Pulls []model.Pull
	Meta  Meta
	// Err is set when the pull requests of the repo could not be read.
	Err error
}

// Meta describes where the data of a result came from.
type Meta struct {
	// FetchedAt is when the provider last confirmed the data.
	FetchedAt time.Time
	// Stale is true when the data is cached because the provider could not be asked.
	Stale bool
	// StaleReason matches gitprovider.ErrQuotaReserve or gitprovider.ErrMaxRetry.
	StaleReason error
}

// Merge combines the metadata of two requests that feed one result: the older
// FetchedAt wins and the result is stale when either part is.
func (m Meta) Merge(other Meta) Meta {
	switch {
	case m.FetchedAt.IsZero():
		return other
	case other.FetchedAt.IsZero():
		return m
	}
	merged := m
	if other.FetchedAt.Before(m.FetchedAt) {
		merged.FetchedAt = other.FetchedAt
	}
	if !m.Stale && other.Stale {
		merged.Stale, merged.StaleReason = true, other.StaleReason
	}
	return merged
}

// GraphQLError is one entry of the errors member of a GraphQL response.
type GraphQLError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// GraphQLErrors is returned when a GraphQL response carries errors and no data.
type GraphQLErrors []GraphQLError

// Error implements error.
func (e GraphQLErrors) Error() string {
	messages := make([]string, len(e))
	for i, ge := range e {
		messages[i] = ge.Message
	}
	return "graphql: " + strings.Join(messages, "; ")
}

// Is reports whether target is ErrNotFound and the provider reported NOT_FOUND.
func (e GraphQLErrors) Is(target error) bool {
	if target != ErrNotFound {
		return false
	}
	for _, ge := range e {
		if ge.Type == "NOT_FOUND" {
			return true
		}
	}
	return false
}

// Query sends a GraphQL request and decodes the data member into T. A provider
// status outside 2xx becomes a *gitprovider.StatusError. Errors that come with
// partial data are returned next to the data, so callers can judge per item.
func Query[T any](ctx context.Context, c *client.Client, req client.GraphQLRequest) (*T, GraphQLErrors, Meta, error) {
	resp, err := c.GraphQL(ctx, req)
	if err != nil {
		return nil, nil, Meta{}, err
	}
	meta := Meta{FetchedAt: resp.FetchedAt, Stale: resp.Stale, StaleReason: resp.StaleReason}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, nil, meta, &gitprovider.StatusError{StatusCode: resp.StatusCode}
	}
	var envelope struct {
		Data   *T            `json:"data"`
		Errors GraphQLErrors `json:"errors"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return nil, nil, meta, fmt.Errorf("provider: decode graphql response: %w", err)
	}
	if envelope.Data == nil && len(envelope.Errors) > 0 {
		return nil, nil, meta, envelope.Errors
	}
	if envelope.Data == nil {
		return nil, nil, meta, errors.New("provider: graphql response has no data")
	}
	return envelope.Data, envelope.Errors, meta, nil
}

// AbsoluteURL resolves a possibly relative link, such as a GitLab avatar path,
// against base.
func AbsoluteURL(base, link string) string {
	if strings.HasPrefix(link, "/") {
		return strings.TrimRight(base, "/") + link
	}
	return link
}

// Get sends a REST GET under the client policy. A 404 becomes ErrNotFound and any other
// status outside 2xx a *gitprovider.StatusError.
func Get(ctx context.Context, c *client.Client, req client.Request) (*client.Response, Meta, error) {
	resp, err := c.Get(ctx, req)
	if err != nil {
		return nil, Meta{}, err
	}
	meta := Meta{FetchedAt: resp.FetchedAt, Stale: resp.Stale, StaleReason: resp.StaleReason}
	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		return resp, meta, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, meta, ErrNotFound
	default:
		return nil, meta, &gitprovider.StatusError{StatusCode: resp.StatusCode}
	}
}

// Pages reads the numbered pages of a REST list, starting at page 1, and decodes each into
// []T. It stops at the first page with fewer than perPage items. When maxPages are read and
// the last one was full, more items may exist and truncated is true. Pagination relies on
// the page size, not on Link headers, because cached responses carry no such headers.
func Pages[T any](ctx context.Context, c *client.Client, perPage, maxPages int,
	request func(page int) client.Request) (items []T, truncated bool, meta Meta, err error) {
	for page := 1; ; page++ {
		resp, pageMeta, err := Get(ctx, c, request(page))
		if err != nil {
			return nil, false, Meta{}, err
		}
		meta = meta.Merge(pageMeta)
		var batch []T
		if err := json.Unmarshal(resp.Body, &batch); err != nil {
			return nil, false, Meta{}, fmt.Errorf("provider: decode page %d: %w", page, err)
		}
		items = append(items, batch...)
		switch {
		case len(batch) < perPage:
			return items, false, meta, nil
		case page >= maxPages:
			return items, true, meta, nil
		}
	}
}

// Decode unmarshals a REST response body into v.
func Decode(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("provider: decode response: %w", err)
	}
	return nil
}
