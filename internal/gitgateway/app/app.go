// Package app wires the git-gateway components together and runs the server.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"pdd/services/internal/gitgateway/api"
	"pdd/services/internal/gitgateway/auth"
	"pdd/services/internal/gitgateway/backend"
	"pdd/services/internal/gitgateway/config"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/provider/github"
	"pdd/services/internal/gitgateway/provider/gitlab"
	"pdd/services/internal/gitgateway/service"
	"pdd/services/internal/gitgateway/store"
	"pdd/services/internal/gitgateway/upstream"
)

const purgeInterval = time.Hour

// App is the assembled service.
type App struct {
	cfg    config.Config
	log    *slog.Logger
	db     *sql.DB
	cache  *store.CacheStore
	server *http.Server
}

// New opens the database and builds the HTTP handler.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	db, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cipher, err := store.NewCipher(cfg.TokenKey)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	cacheStore := store.NewCacheStore(db, nil)
	client, err := upstream.NewClient(cfg, cacheStore, func(err error) { log.Warn("cache error", "error", err) })
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: create upstream client: %w", err)
	}
	pulls := service.New(map[model.Provider]provider.Provider{
		model.GitHub: github.New(github.Config{
			APIURL: cfg.Provider.GitHubAPIURL, Client: client,
			BatchSize: cfg.Provider.ListBatchSize, MaxPages: cfg.Provider.ListMaxPages,
			ListTTL: cfg.Cache.TTLPullList, DetailTTL: cfg.Cache.TTLPullDetail, ExpectedCost: cfg.Upstream.ExpectedCost,
			TreeTTL: cfg.Cache.TTLTree, FileTTL: cfg.Cache.TTLFile, FilesMaxPages: cfg.Files.MaxPages,
		}),
		model.GitLab: gitlab.New(gitlab.Config{
			Client:    client,
			BatchSize: cfg.Provider.ListBatchSize, MaxPages: cfg.Provider.ListMaxPages,
			ListTTL: cfg.Cache.TTLPullList, DetailTTL: cfg.Cache.TTLPullDetail, ExpectedCost: cfg.Upstream.ExpectedCost,
			TreeTTL: cfg.Cache.TTLTree, FileTTL: cfg.Cache.TTLFile, FilesMaxPages: cfg.Files.MaxPages,
		}),
	}, nil, service.Limits{PatchBytes: cfg.Upstream.MaxBodyBytes, FileBytes: cfg.Files.MaxBytes})
	handler := api.NewHandler(api.Deps{
		Verifier:      auth.NewVerifier(cfg.JWTSecret, nil),
		InternalToken: cfg.InternalToken,
		Backend: backend.New(backend.Config{
			BaseURL: cfg.Backend.URL, InsecureTLS: cfg.Backend.InsecureTLS,
			Timeout: cfg.Backend.Timeout, CacheTTL: cfg.Backend.CacheTTL,
		}),
		Tokens: store.NewTokenStore(db, cipher, nil),
		Pulls:  pulls,
		Health: db.PingContext,
		Logger: log,
	})
	return &App{
		cfg: cfg, log: log, db: db, cache: cacheStore,
		server: &http.Server{
			Addr:              cfg.ListenAddr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			WriteTimeout:      2 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		},
	}, nil
}

// Handler returns the HTTP handler, for tests.
func (a *App) Handler() http.Handler { return a.server.Handler }

// Close releases the database.
func (a *App) Close() error { return a.db.Close() }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	go a.purgeLoop(ctx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- a.server.ListenAndServe() }()
	a.log.Info("git-gateway listening", "addr", a.cfg.ListenAddr)

	select {
	case err := <-serveErr:
		return fmt.Errorf("app: serve: %w", err)
	case <-ctx.Done():
	}
	a.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.cfg.ShutdownTimeout)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("app: shutdown: %w", err)
	}
	return nil
}

// purgeLoop removes cache entries past their stale lifetime.
func (a *App) purgeLoop(ctx context.Context) {
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		n, err := a.cache.PurgeExpired(ctx)
		if err != nil && ctx.Err() == nil {
			a.log.Warn("purge cache", "error", err)
		} else if n > 0 {
			a.log.Info("purged expired cache entries", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
