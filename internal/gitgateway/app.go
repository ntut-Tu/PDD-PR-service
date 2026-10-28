// Package gitgateway wires the git-gateway components together and runs the server.
package gitgateway

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
	"pdd/services/internal/gitgateway/database"
	"pdd/services/internal/gitgateway/github"
	"pdd/services/internal/gitgateway/gitlab"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/pulls"
	"pdd/services/internal/gitgateway/repos"
	"pdd/services/internal/gitgateway/upstream"
)

const purgeInterval = time.Hour

// App owns the assembled server and its resources.
type App struct {
	cfg    config.Config
	log    *slog.Logger
	db     *sql.DB
	cache  *upstream.CacheStore
	server *http.Server
}

// New opens the database and assembles the gateway components.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	now := time.Now

	// create database
	db, err := database.Open(ctx, cfg.DataDir)
	if err != nil {
		return nil, err
	}

	// create auth (using token from cfg)
	cipher, err := auth.NewCipher(cfg.TokenKey)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	// start cache store
	cacheStore := upstream.NewCacheStore(db, now)

	// create func as var
	onCacheError := func(err error) {
		log.Warn("cache error", "error", err)
	}

	// create upstream who owns provider, request execution and response caching.
	upstreamClient, err := newUpstreamClient(
		cfg,
		cacheStore,
		onCacheError) // inject error log func
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: create upstream client: %w", err)
	}

	// providers
	githubProvider := github.New(
		github.Config{
			APIURL:        cfg.Provider.GitHubAPIURL,
			Client:        upstreamClient,
			BatchSize:     cfg.Provider.ListBatchSize,
			MaxPages:      cfg.Provider.ListMaxPages,
			ListTTL:       cfg.Cache.TTLPullList,
			DetailTTL:     cfg.Cache.TTLPullDetail,
			ExpectedCost:  cfg.Upstream.ExpectedCost,
			TreeTTL:       cfg.Cache.TTLTree,
			FileTTL:       cfg.Cache.TTLFile,
			FilesMaxPages: cfg.Files.MaxPages,
		})

	gitlabProvider := gitlab.New(gitlab.Config{
		Client:        upstreamClient,
		BatchSize:     cfg.Provider.ListBatchSize,
		MaxPages:      cfg.Provider.ListMaxPages,
		ListTTL:       cfg.Cache.TTLPullList,
		DetailTTL:     cfg.Cache.TTLPullDetail,
		ExpectedCost:  cfg.Upstream.ExpectedCost,
		TreeTTL:       cfg.Cache.TTLTree,
		FileTTL:       cfg.Cache.TTLFile,
		FilesMaxPages: cfg.Files.MaxPages,
	})

	contentProviders := map[model.Provider]repos.Provider{
		model.GitHub: githubProvider,
		model.GitLab: gitlabProvider,
	}

	repositories := repos.New(contentProviders, now, cfg.Files.MaxBytes)

	pullProviders := map[model.Provider]pulls.Provider{
		model.GitHub: githubProvider,
		model.GitLab: gitlabProvider,
	}

	pullRequests := pulls.New(
		pullProviders,
		repositories,
		now,
		cfg.Upstream.MaxBodyBytes,
	)

	verifier := auth.NewVerifier(cfg.JWTSecret, now)
	tokens := auth.NewTokenStore(db, cipher, now)

	backendClient := backend.New(backend.Config{
		BaseURL:     cfg.Backend.URL,
		InsecureTLS: cfg.Backend.InsecureTLS,
		Timeout:     cfg.Backend.Timeout,
		CacheTTL:    cfg.Backend.CacheTTL,
	})

	handler := api.NewHandler(api.Deps{
		Verifier:      verifier,
		InternalToken: cfg.InternalToken,
		Backend:       backendClient,
		Tokens:        tokens,
		Pulls:         pullRequests,
		Repos:         repositories,
		Health:        db.PingContext,
		Logger:        log,
	})

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	return &App{
		cfg:    cfg,
		log:    log,
		db:     db,
		cache:  cacheStore,
		server: server,
	}, nil
}

// Handler returns the assembled HTTP handler.
func (a *App) Handler() http.Handler {
	return a.server.Handler
}

// Close releases the database.
func (a *App) Close() error {
	return a.db.Close()
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	go a.purgeLoop(ctx) // purgeLoop removes cache entries past their stale lifetime.

	serveErr := make(chan error, 1) // create a error channel with "1" buffer
	go func() {
		serveErr <- a.server.ListenAndServe() // send err to channel "serveErr" (background)
	}()

	a.log.Info("git-gateway listening", "addr", a.cfg.ListenAddr)

	// if error fmt.Errorf; else wait until normal shutdown
	select {
	case err := <-serveErr:
		return fmt.Errorf("app: serve: %w", err)
	case <-ctx.Done():
	}

	a.log.Info("shutting down")

	// "Graceful Shutdown": stop accept request and wait until all process finished
	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), // copy the old ctx values to create new context to prevent old ctx Shutdown being canceled
		a.cfg.ShutdownTimeout,      // if timeout Shutdown will be canceled
	)
	defer cancel()

	if err := a.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("app: shutdown: %w", err)
	}

	return nil
}

// purgeLoop removes cache entries past their stale lifetime.
func (a *App) purgeLoop(ctx context.Context) {
	// this function can be removed, not really need to clean
	ticker := time.NewTicker(purgeInterval) // "ticker": auto triggered
	defer ticker.Stop()

	for {
		n, err := a.cache.PurgeExpired(ctx)
		if err != nil && ctx.Err() == nil {
			a.log.Warn("purge cache", "error", err)
		} else if n > 0 {
			a.log.Info("purged expired cache entries", "count", n)
		}

		// this select is used as block, until ticker triggered loop continue
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
