package proxy_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"pdd/services/internal/sandboxcontroller/proxy"
)

// startUpstream runs a TCP echo server and returns its address.
func startUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func newProxy(t *testing.T, hosts []string, dialed *[]string, dial proxy.Dialer) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(proxy.New(proxy.Config{AllowedHosts: hosts, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if dialed != nil {
			*dialed = append(*dialed, addr)
		}
		return dial(ctx, network, addr)
	}}))
	t.Cleanup(srv.Close)
	return srv
}

func connect(t *testing.T, proxyAddr, target string) (net.Conn, *http.Response) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn, resp
}

func TestConnectToAllowedHostTunnelsBytes(t *testing.T) {
	upstream := startUpstream(t)
	var dialed []string
	srv := newProxy(t, []string{"repo.example.com"}, &dialed, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream)
	})

	conn, resp := connect(t, srv.Listener.Addr().String(), "repo.example.com:443")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo = %q, %v", buf, err)
	}
	if len(dialed) != 1 || dialed[0] != "repo.example.com:443" {
		t.Errorf("dialed = %v", dialed)
	}
}

func TestConnectDefaultsToPort443AndIsCaseInsensitive(t *testing.T) {
	var dialed []string
	srv := newProxy(t, []string{"repo.example.com"}, &dialed, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("down")
	})

	_, resp := connect(t, srv.Listener.Addr().String(), "REPO.Example.com")

	if resp.StatusCode != http.StatusBadGateway || len(dialed) != 1 || dialed[0] != "REPO.Example.com:443" && dialed[0] != "repo.example.com:443" {
		t.Errorf("status = %d, dialed = %v", resp.StatusCode, dialed)
	}
}

func TestDeniedTargetsAreNeverDialed(t *testing.T) {
	var dialed []string
	srv := newProxy(t, []string{"repo.example.com", "*.gradle.org"}, &dialed, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	})
	for _, target := range []string{
		"evil.example.org:443", "repo.example.com:22", "repo.example.com:8080", "127.0.0.1:443",
		"gradle.org:443", "notgradle.org:443", "repo.example.com.evil.com:443", "[::1]:443",
	} {
		t.Run(target, func(t *testing.T) {
			_, resp := connect(t, srv.Listener.Addr().String(), target)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d", resp.StatusCode)
			}
		})
	}
	if len(dialed) != 0 {
		t.Errorf("dialed %v", dialed)
	}
}

func TestWildcardMatchesSubdomains(t *testing.T) {
	p := proxy.New(proxy.Config{AllowedHosts: []string{"*.gradle.org", "Repo.Example.com"}})
	for host, want := range map[string]bool{
		"plugins.gradle.org": true, "a.b.gradle.org": true, "gradle.org": false, "xgradle.org": false,
		"repo.example.com": true, "repo.example.com.": true, "other.com": false, "": false,
	} {
		if got := p.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v", host, got)
		}
	}
}

func proxyClient(proxyURL string) *http.Client {
	u, _ := url.Parse(proxyURL)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 5 * time.Second}
}

func TestPlainHTTPIsForwardedForAllowedHost(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Proxy-Auth", r.Header.Get("Proxy-Authorization"))
		_, _ = fmt.Fprintf(w, "hello %s %s", r.Method, r.URL.Path)
	}))
	defer origin.Close()
	var dialed []string
	srv := newProxy(t, []string{"mirror.example.com"}, &dialed, func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, origin.Listener.Addr().String())
	})

	req, _ := http.NewRequest(http.MethodGet, "http://mirror.example.com/a/b.pom", nil)
	req.Header.Set("Proxy-Authorization", "secret")
	resp, err := proxyClient(srv.URL).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 || string(body) != "hello GET /a/b.pom" || resp.Header.Get("X-Seen-Proxy-Auth") != "" {
		t.Errorf("status %d body %q headers %v", resp.StatusCode, body, resp.Header)
	}
	if len(dialed) != 1 || dialed[0] != "mirror.example.com:80" {
		t.Errorf("dialed = %v", dialed)
	}
}

func TestPlainHTTPToDeniedHostOrPortIsForbidden(t *testing.T) {
	srv := newProxy(t, []string{"mirror.example.com"}, nil, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	})
	for _, target := range []string{"http://evil.example.org/x", "http://mirror.example.com:8080/x"} {
		resp, err := proxyClient(srv.URL).Get(target)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d", target, resp.StatusCode)
		}
	}
}

func TestPlainHTTPUpstreamFailureIsBadGateway(t *testing.T) {
	srv := newProxy(t, []string{"mirror.example.com"}, nil, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("down")
	})

	resp, err := proxyClient(srv.URL).Get("http://mirror.example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestOriginFormRequestsAndHTTPSURLsAreRejected(t *testing.T) {
	srv := newProxy(t, []string{"mirror.example.com"}, nil, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	})

	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", resp.StatusCode)
	}

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("GET https://mirror.example.com/x HTTP/1.1\r\nHost: mirror.example.com\r\n\r\n"))
	r, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || r.StatusCode != http.StatusBadRequest {
		t.Errorf("https absolute URI: %v %v", r, err)
	}
}

func TestDefaultDialerReportsUnreachableUpstream(t *testing.T) {
	// Port 443 on localhost is not allowed by name, so use an allowed name that cannot resolve.
	srv := httptest.NewServer(proxy.New(proxy.Config{AllowedHosts: []string{"nonexistent.invalid"}}))
	defer srv.Close()

	_, resp := connect(t, srv.Listener.Addr().String(), "nonexistent.invalid:443")

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(http.StatusText(resp.StatusCode), "Gateway") {
		t.Error("unexpected status text")
	}
}
