// Package app wires the sandbox-controller components together and runs the servers and the
// reconcile loop.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"pdd/services/internal/sandboxcontroller/api"
	"pdd/services/internal/sandboxcontroller/config"
	"pdd/services/internal/sandboxcontroller/proxy"
	"pdd/services/internal/sandboxcontroller/reconcile"
	"pdd/services/internal/sandboxcontroller/runtime"
	"pdd/services/internal/sandboxcontroller/runtime/containerd"
	"pdd/services/internal/sandboxcontroller/source"
	"pdd/services/internal/sandboxcontroller/store"
	"pdd/services/internal/sandboxcontroller/workspace"
)

// App is the assembled service.
type App struct {
	cfg        config.Config
	log        *slog.Logger
	db         *sql.DB
	controller *reconcile.Controller
	api        *http.Server
	proxy      *http.Server
	apiLn      net.Listener
	proxyLn    net.Listener
	// closeRuntime releases a runtime that New created itself.
	closeRuntime func() error
}

type options struct {
	runtime     runtime.Runtime
	ociRuntime  string
	proxyDialer proxy.Dialer
	now         func() time.Time
}

// Option customizes New. The options exist for tests; the binary uses none of them.
type Option func(*options)

// WithRuntime replaces the containerd runtime, for example with a fake.
func WithRuntime(rt runtime.Runtime) Option { return func(o *options) { o.runtime = rt } }

// WithOCIRuntime replaces runsc. Only tests use it.
func WithOCIRuntime(name string) Option { return func(o *options) { o.ociRuntime = name } }

// WithProxyDialer replaces the dialer the allow-list proxy uses for upstream connections.
func WithProxyDialer(d proxy.Dialer) Option { return func(o *options) { o.proxyDialer = d } }

// WithClock replaces the time source of the reconcile loop.
func WithClock(now func() time.Time) Option { return func(o *options) { o.now = now } }

// New opens the database, binds the listeners and builds the handlers.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Option) (*App, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, log: log, db: db}
	if a.apiLn, err = net.Listen("tcp", cfg.ListenAddr); err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("app: listen on %s: %w", cfg.ListenAddr, err)
	}
	if a.proxyLn, err = net.Listen("tcp", cfg.Network.ProxyListen); err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("app: listen proxy on %s: %w", cfg.Network.ProxyListen, err)
	}
	proxyPort := a.proxyLn.Addr().(*net.TCPAddr).Port
	if o.runtime == nil {
		if o.runtime, a.closeRuntime, err = newRuntime(cfg.Runtime, proxyPort); err != nil {
			_ = a.Close()
			return nil, err
		}
	}
	runs := store.NewRuns(db)
	a.controller = reconcile.New(reconcile.Deps{
		Store:   runs,
		Runtime: o.runtime,
		Workspaces: &workspace.Manager{
			Root: cfg.Workspace.Dir, MaxBytes: cfg.Workspace.MaxBytes, MaxFiles: cfg.Workspace.MaxFiles,
		},
		Source: source.New(cfg.GitGateway.URL, cfg.GitGateway.InternalToken, cfg.GitGateway.Timeout),
		Log:    log,
		Now:    o.now,
	}, reconcile.Settings{
		OCIRuntime:    o.ociRuntime,
		MaxConcurrent: cfg.Limits.MaxConcurrent, Timeout: cfg.Limits.Timeout,
		CPUs: cfg.Limits.CPUs, MemoryBytes: cfg.Limits.MemoryBytes, Pids: int64(cfg.Limits.Pids), User: cfg.Limits.User,
		NetworkName: cfg.Network.Name, ProxyHost: cfg.Network.ProxyHost,
		ProxyPort:  strconv.Itoa(proxyPort),
		ImageMaven: cfg.Images.Maven, ImageGradle: cfg.Images.Gradle,
		CacheDir: cfg.CacheDir, OutputMaxBytes: cfg.OutputMaxBytes, WorkspaceRetention: cfg.WorkspaceRetention,
	})
	a.api = &http.Server{
		Handler: api.NewHandler(api.Deps{
			InternalToken: cfg.InternalToken, Runs: runs, Trigger: a.controller.Trigger,
			SandboxAvailable: a.controller.SandboxAvailable, Logger: log,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	a.proxy = &http.Server{
		Handler:           proxy.New(proxy.Config{AllowedHosts: cfg.Network.AllowedHosts, Dial: o.proxyDialer, Logger: log}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return a, nil
}

// newRuntime builds the containerd runtime. The second result releases it.
func newRuntime(cfg config.Runtime, proxyPort int) (runtime.Runtime, func() error, error) {
	c := cfg.Containerd
	client, err := containerd.New(containerd.Config{
		Address: c.Address, Namespace: c.Namespace, Shim: c.Shim, RunscPath: c.RunscPath, LogDir: c.LogDir,
		NetnsDir: c.NetnsDir, CNIBinDir: c.CNIBinDir, CNIDataDir: c.CNIDataDir, CNISubnet: c.CNISubnet, ProxyPort: proxyPort,
	})
	if err != nil {
		return nil, nil, err
	}
	return client, client.Close, nil
}

// Handler returns the API handler, for tests.
func (a *App) Handler() http.Handler { return a.api.Handler }

// APIAddr is the address the API listens on.
func (a *App) APIAddr() string { return a.apiLn.Addr().String() }

// ProxyAddr is the address the allow-list proxy listens on.
func (a *App) ProxyAddr() string { return a.proxyLn.Addr().String() }

// Close releases the database and the listeners.
func (a *App) Close() error {
	var errs []error
	for _, ln := range []net.Listener{a.apiLn, a.proxyLn} {
		if ln != nil {
			if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, a.db.Close())
	if a.closeRuntime != nil {
		errs = append(errs, a.closeRuntime())
	}
	return errors.Join(errs...)
}

// Run serves until ctx is cancelled, then shuts down gracefully. Containers of running
// sandboxes are left alone: the next process adopts them.
func (a *App) Run(ctx context.Context) error {
	if !a.controller.SandboxAvailable(ctx) {
		a.log.Warn("sandboxes are not available: runs will end as sandbox_unavailable until the container runtime is reachable and runsc is installed")
	}
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		a.controller.Run(loopCtx, a.cfg.ResyncInterval)
	}()

	serveErr := make(chan error, 2)
	go func() { serveErr <- a.api.Serve(a.apiLn) }()
	go func() { serveErr <- a.proxy.Serve(a.proxyLn) }()
	a.log.Info("sandbox-controller listening", "api", a.APIAddr(), "proxy", a.ProxyAddr())

	var runErr error
	select {
	case err := <-serveErr:
		runErr = fmt.Errorf("app: serve: %w", err)
	case <-ctx.Done():
	}
	a.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancel()
	for _, srv := range []*http.Server{a.api, a.proxy} {
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Open proxy tunnels outlive the timeout; drop them.
			_ = srv.Close()
		}
	}
	stopLoop()
	<-loopDone
	return runErr
}
