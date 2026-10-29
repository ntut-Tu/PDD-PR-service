// Package proxy implements the allow-list HTTP proxy that is the only way out of a sandbox: an
// HTTP CONNECT tunnel for https:// repositories and a plain forward for http:// ones, both
// limited to configured host names and to ports 80 and 443.
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const dialTimeout = 15 * time.Second

// Dialer opens connections to the upstream hosts.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Config configures a Handler.
type Config struct {
	// AllowedHosts are lower-case host names; "*.example.com" matches every subdomain.
	AllowedHosts []string
	// Dial defaults to a net.Dialer with a 15 second timeout.
	Dial   Dialer
	Logger *slog.Logger
}

// Handler is the proxy as an http.Handler.
type Handler struct {
	exact    map[string]bool
	suffixes []string
	dial     Dialer
	log      *slog.Logger
	forward  *http.Transport
}

// New builds the proxy.
func New(cfg Config) *Handler {
	h := &Handler{exact: map[string]bool{}, dial: cfg.Dial, log: cfg.Logger}
	for _, host := range cfg.AllowedHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if rest, ok := strings.CutPrefix(host, "*."); ok {
			h.suffixes = append(h.suffixes, "."+rest)
		} else {
			h.exact[host] = true
		}
	}
	if h.dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		h.dial = d.DialContext
	}
	if h.log == nil {
		h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h.forward = &http.Transport{DialContext: h.dial, Proxy: nil, ResponseHeaderTimeout: 2 * time.Minute, DisableCompression: true}
	return h
}

// Allowed reports whether host (no port) may be reached.
func (h *Handler) Allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h.exact[host] {
		return true
	}
	for _, s := range h.suffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		h.connect(w, r)
		return
	}
	if r.URL.IsAbs() && r.URL.Scheme == "http" {
		h.plain(w, r)
		return
	}
	http.Error(w, "this is a proxy: send absolute http:// requests or CONNECT", http.StatusBadRequest)
}

// target splits host:port, defaulting to defaultPort, and checks both against the allow list.
func (h *Handler) target(hostport, defaultPort string) (string, bool) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, defaultPort
	}
	if (port != "80" && port != "443") || !h.Allowed(host) {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	addr, ok := h.target(r.Host, "443")
	if !ok {
		h.deny(w, r.Host)
		return
	}
	upstream, err := h.dial(r.Context(), "tcp", addr)
	if err != nil {
		h.log.Warn("proxy dial failed", "target", addr, "error", err)
		http.Error(w, "cannot reach upstream", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "tunnel not supported", http.StatusInternalServerError)
		return
	}
	client, buf, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	_, _ = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if err := buf.Flush(); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	h.log.Debug("proxy tunnel", "target", addr)
	tunnel(client, upstream)
}

// tunnel copies both directions until either side closes.
func tunnel(a, b net.Conn) {
	var wg sync.WaitGroup
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

var hopHeaders = []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (h *Handler) plain(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.target(r.URL.Host, "80"); !ok {
		h.deny(w, r.URL.Host)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	for _, name := range hopHeaders {
		out.Header.Del(name)
	}
	resp, err := h.forward.RoundTrip(out)
	if err != nil {
		h.log.Warn("proxy forward failed", "host", r.URL.Host, "error", err)
		http.Error(w, "cannot reach upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, name := range hopHeaders {
		resp.Header.Del(name)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (h *Handler) deny(w http.ResponseWriter, target string) {
	h.log.Info("proxy denied", "target", target)
	http.Error(w, "host is not on the sandbox allow list", http.StatusForbidden)
}
