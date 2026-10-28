// Package providertest helps testing provider adapters against fake servers.
package providertest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pdd/services/internal/gitgateway/upstream"
)

// NewClient returns a policy client with an in-memory cache and a retrier that
// does not sleep.
func NewClient(t *testing.T) *upstream.Client {
	t.Helper()
	c, err := upstream.New(upstream.Config{
		Cache:      upstream.NewMemoryStore(0, nil),
		Limiter:    upstream.NewQuotaTracker(upstream.DefaultQuotaConfig()),
		Retrier:    &upstream.Retrier{Policy: upstream.DefaultPolicy(), Sleep: func(context.Context, time.Duration) error { return nil }},
		DefaultTTL: time.Minute,
		StaleTTL:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// GraphQLRequest is a decoded request received by a Server.
type GraphQLRequest struct {
	Path          string
	Authorization string
	Query         string
	Variables     map[string]any
}

// Server is a fake GraphQL endpoint that answers with a caller-provided function.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []GraphQLRequest
}

// NewServer starts a Server; respond writes the answer for each request.
func NewServer(t *testing.T, respond func(req GraphQLRequest, w http.ResponseWriter)) *Server {
	t.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		req := GraphQLRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Query: payload.Query, Variables: payload.Variables}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		respond(req, w)
	}))
	t.Cleanup(s.Close)
	return s
}

// Requests returns the requests received so far.
func (s *Server) Requests() []GraphQLRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]GraphQLRequest(nil), s.requests...)
}

// JSON writes body as a 200 JSON response.
func JSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// RESTRequest is a decoded REST request received by a RESTServer.
type RESTRequest struct {
	// Path is the escaped request path, so %2F stays visible.
	Path          string
	Query         string
	Authorization string
	Accept        string
	IfNoneMatch   string
}

// RESTServer is a fake REST API that answers with a caller-provided function.
type RESTServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []RESTRequest
}

// NewRESTServer starts a RESTServer; respond writes the answer for each request.
func NewRESTServer(t *testing.T, respond func(req RESTRequest, w http.ResponseWriter)) *RESTServer {
	t.Helper()
	s := &RESTServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := RESTRequest{
			Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Authorization: r.Header.Get("Authorization"),
			Accept: r.Header.Get("Accept"), IfNoneMatch: r.Header.Get("If-None-Match"),
		}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		respond(req, w)
	}))
	t.Cleanup(s.Close)
	return s
}

// Requests returns the requests received so far.
func (s *RESTServer) Requests() []RESTRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RESTRequest(nil), s.requests...)
}
