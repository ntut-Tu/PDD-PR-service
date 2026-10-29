package client_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pdd/services/pkg/gitprovider"
	"pdd/services/pkg/gitprovider/client"
)

func stream(t *testing.T, f *fixture, url string) (*client.StreamResponse, error) {
	t.Helper()
	return f.client.Stream(context.Background(), client.Request{URL: url, Token: "secret-token"})
}

func TestStreamReturnsTheOpenBodyAndSendsTheToken(t *testing.T) {
	var auth string
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "archive-bytes")
	})
	f := newFixture(t)

	resp, err := stream(t, f, srv.URL+"/a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "archive-bytes" || resp.StatusCode != http.StatusOK || resp.ContentLength != 13 || auth != "Bearer secret-token" {
		t.Errorf("body %q status %d length %d auth %q", body, resp.StatusCode, resp.ContentLength, auth)
	}
}

func TestStreamIsNotCachedAndIgnoresTheBodyLimit(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "0123456789") })
	f := newFixture(t, func(c *client.Config) { c.MaxBodyBytes = 4 })

	for range 2 {
		resp, err := stream(t, f, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "0123456789" {
			t.Fatalf("body = %q", body)
		}
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
}

func TestStreamFollowsRedirectsWithoutForwardingTheToken(t *testing.T) {
	var redirectedAuth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "from-codeload")
	}))
	t.Cleanup(target.Close)
	srv, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		// The port is ignored when hosts are compared, so the target is addressed by another name.
		http.Redirect(w, r, strings.Replace(target.URL, "127.0.0.1", "localhost", 1)+"/signed", http.StatusFound)
	})
	f := newFixture(t)

	resp, err := stream(t, f, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "from-codeload" {
		t.Errorf("body = %q", body)
	}
	if got, _ := redirectedAuth.Load().(string); got != "" {
		t.Errorf("redirect target received Authorization %q", got)
	}
}

func TestStreamRetriesServerErrorsAndHonorsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	f := newFixture(t)

	resp, err := stream(t, f, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if calls.Load() != 2 || len(f.clock.Slept()) != 1 || f.clock.Slept()[0] != 3*time.Second {
		t.Errorf("calls %d slept %v", calls.Load(), f.clock.Slept())
	}
}

func TestStreamMaxRetryReached(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	f := newFixture(t)

	_, err := stream(t, f, srv.URL)

	if !errors.Is(err, gitprovider.ErrMaxRetry) || hits.Load() != 3 {
		t.Errorf("err %v hits %d", err, hits.Load())
	}
}

func TestStreamRetriesNetworkError(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	var attempts atomic.Int32
	f := newFixture(t, func(c *client.Config) {
		c.StreamHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, errors.New("connection reset by peer")
			}
			return http.DefaultTransport.RoundTrip(r)
		})}
	})

	resp, err := stream(t, f, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d", attempts.Load())
	}
}

func TestStreamFinalStatusesCarryNoBody(t *testing.T) {
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	f := newFixture(t)

	resp, err := stream(t, f, srv.URL)

	if err != nil || resp.StatusCode != http.StatusNotFound || resp.Body != nil || hits.Load() != 1 {
		t.Fatalf("resp %+v err %v hits %d", resp, err, hits.Load())
	}
}

func TestStreamUnauthorized(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })

	_, err := stream(t, newFixture(t), srv.URL)

	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Errorf("err = %v", err)
	}
}

func TestStreamQuotaReserveSendsNothing(t *testing.T) {
	reset := epoch.Add(30 * time.Minute)
	srv, hits := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		quotaHeaders(w, 5000, 120, reset)
		_, _ = io.WriteString(w, "ok")
	})
	f := newFixture(t)
	if _, err := get(t, f, srv.URL+"/a"); err != nil {
		t.Fatal(err)
	}

	_, err := stream(t, f, srv.URL+"/b")

	if !errors.Is(err, gitprovider.ErrQuotaReserve) || hits.Load() != 1 {
		t.Errorf("err %v hits %d", err, hits.Load())
	}
}

func TestStreamRejectsInvalidURLAndCancelledContext(t *testing.T) {
	f := newFixture(t)
	if _, err := stream(t, f, "not a url"); err == nil {
		t.Error("invalid URL accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.client.Stream(ctx, client.Request{URL: "http://127.0.0.1:1/"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
