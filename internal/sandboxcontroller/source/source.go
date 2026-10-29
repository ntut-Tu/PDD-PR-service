// Package source downloads repository archives from git-gateway. The gateway holds the provider
// token; this client only sends the shared internal token.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
)

// ErrUnavailable means the archive could not be obtained; the run ends as source_unavailable.
var ErrUnavailable = errors.New("source unavailable")

// Client calls the internal archive endpoint of git-gateway.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client. timeout bounds one whole download.
func New(baseURL, internalToken string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   internalToken,
		http:    &http.Client{Timeout: timeout},
	}
}

// Archive opens the tar.gz archive of the repository at the spec's commit. The caller closes it.
func (c *Client) Archive(ctx context.Context, spec model.Spec) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/internal/v1/projects/%d/repos/%d/archive?%s", c.baseURL, spec.ProjectID, spec.RepoID,
		url.Values{"account": {spec.Account}, "ref": {spec.HeadSHA}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("X-Internal-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: git-gateway request failed: %v", ErrUnavailable, urlErrorCause(err))
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, fmt.Errorf("%w: git-gateway answered %s%s", ErrUnavailable, strconv.Itoa(resp.StatusCode), gatewayMessage(resp.Body))
	}
	return resp.Body, nil
}

// urlErrorCause drops the request URL, which contains the account name, from transport errors.
func urlErrorCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// gatewayMessage extracts the message of a git-gateway error body, if there is one.
func gatewayMessage(body io.Reader) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&e); err != nil || e.Error.Message == "" {
		return ""
	}
	return ": " + e.Error.Message
}
