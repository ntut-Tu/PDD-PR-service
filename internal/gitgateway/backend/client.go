// Package backend is the client of the PDD backend used to authorize project
// access and to resolve the repositories bound to a project.
package backend

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"pdd/services/internal/gitgateway/model"
)

const maxResponseBytes = 4 << 20

// ErrUnavailable is matched by errors.Is when the backend cannot be reached or
// answers with an unexpected status.
var ErrUnavailable = errors.New("backend: unavailable")

// Config configures a Client.
type Config struct {
	// BaseURL is the backend root without trailing slash.
	BaseURL string
	// InsecureTLS skips certificate verification for the self-signed backend.
	InsecureTLS bool
	Timeout     time.Duration
	// CacheTTL is how long project and repository lookups are reused.
	CacheTTL time.Duration
	// Now defaults to time.Now.
	Now func() time.Time
}

// Client reads projects and repositories from the PDD backend.
type Client struct {
	cfg      Config
	http     *http.Client
	projects *ttlCache[string, map[int64]struct{}]
	repos    *ttlCache[int64, []model.Repo]
}

// New creates a Client.
func New(cfg Config) *Client {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureTLS {
		// The legacy backend only has a self-signed certificate; the operator opts in explicitly.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &Client{
		cfg:      cfg,
		http:     &http.Client{Transport: transport, Timeout: cfg.Timeout},
		projects: newTTLCache[string, map[int64]struct{}](cfg.CacheTTL, cfg.Now),
		repos:    newTTLCache[int64, []model.Repo](cfg.CacheTTL, cfg.Now),
	}
}

// IsMember reports whether the account belongs to the project. The caller's
// JWT is forwarded to the backend, and the answer is cached per account.
func (c *Client) IsMember(ctx context.Context, jwt, account string, projectID int64) (bool, error) {
	ids, ok := c.projects.get(account)
	if !ok {
		var err error
		if ids, err = c.fetchProjects(ctx, jwt); err != nil {
			return false, err
		}
		c.projects.set(account, ids)
	}
	_, member := ids[projectID]
	return member, nil
}

// Repos returns the GitHub and GitLab repositories bound to the project.
// jwt may be empty, as the backend does not require it for this endpoint.
func (c *Client) Repos(ctx context.Context, jwt string, projectID int64) ([]model.Repo, error) {
	if repos, ok := c.repos.get(projectID); ok {
		return repos, nil
	}
	var entries []repoEntry
	if err := c.getJSON(ctx, jwt, "/repo/"+strconv.FormatInt(projectID, 10), &entries); err != nil {
		return nil, err
	}
	repos := classify(entries)
	c.repos.set(projectID, repos)
	return repos, nil
}

func (c *Client) fetchProjects(ctx context.Context, jwt string) (map[int64]struct{}, error) {
	var projects []struct {
		ID int64 `json:"id"`
	}
	if err := c.getJSON(ctx, jwt, "/project/", &projects); err != nil {
		return nil, err
	}
	ids := make(map[int64]struct{}, len(projects))
	for _, p := range projects {
		ids[p.ID] = struct{}{}
	}
	return ids, nil
}

func (c *Client) getJSON(ctx context.Context, jwt, path string, out any) error {
	endpoint, err := url.JoinPath(c.cfg.BaseURL, path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: GET %s answered %d", ErrUnavailable, path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("%w: decode %s: %w", ErrUnavailable, path, err)
	}
	return nil
}

// ttlCache is a small mutex-guarded cache with a fixed lifetime.
type ttlCache[K comparable, V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[K]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value   V
	expires time.Time
}

func newTTLCache[K comparable, V any](ttl time.Duration, now func() time.Time) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, now: now, entries: make(map[K]ttlEntry[V])}
}

func (c *ttlCache[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !c.now().Before(e.expires) {
		delete(c.entries, key)
		var zero V
		return zero, false
	}
	return e.value, true
}

func (c *ttlCache[K, V]) set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = ttlEntry[V]{value: value, expires: now.Add(c.ttl)}
}
