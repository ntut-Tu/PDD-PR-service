package upstream

import (
	"crypto/sha256"
	"io"
	"net/http"
	"sync"
)

// LimitedTransport caps the simultaneous requests per Authorization header, so
// one user's token never exceeds the provider's concurrency limit.
type LimitedTransport struct {
	next  http.RoundTripper
	limit int

	mu      sync.Mutex
	pending map[[sha256.Size]byte]*slot
}

type slot struct {
	sem  chan struct{}
	refs int
}

// NewLimitedTransport wraps next and allows at most limit concurrent requests per token.
func NewLimitedTransport(next http.RoundTripper, limit int) *LimitedTransport {
	return &LimitedTransport{next: next, limit: limit, pending: make(map[[sha256.Size]byte]*slot)}
}

// RoundTrip implements http.RoundTripper. The slot is held until the response
// body is closed, so a slow body read still counts as an active request.
func (t *LimitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	key := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	s := t.acquireSlot(key)
	release := func() {
		<-s.sem
		t.releaseSlot(key, s)
	}
	select {
	case s.sem <- struct{}{}:
	case <-req.Context().Done():
		t.releaseSlot(key, s)
		return nil, req.Context().Err()
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: sync.OnceFunc(release)}
	return resp, nil
}

// releasingBody frees the concurrency slot when the body is closed.
type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func (t *LimitedTransport) acquireSlot(key [sha256.Size]byte) *slot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.pending[key]
	if !ok {
		s = &slot{sem: make(chan struct{}, t.limit)}
		t.pending[key] = s
	}
	s.refs++
	return s
}

func (t *LimitedTransport) releaseSlot(key [sha256.Size]byte, s *slot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.refs--; s.refs == 0 {
		delete(t.pending, key)
	}
}
