// Package api exposes the gateway over HTTP: the public API used by the PDD
// frontend, the internal API used by other services, and the health check.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"pdd/services/internal/gitgateway/auth"
	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/service"
	"pdd/services/internal/gitgateway/store"
	"pdd/services/pkg/gitprovider"
)

// Backend authorizes project access and resolves repositories.
type Backend interface {
	IsMember(ctx context.Context, jwt, account string, projectID int64) (bool, error)
	Repos(ctx context.Context, jwt string, projectID int64) ([]model.Repo, error)
}

// Tokens persists the provider token of each account.
type Tokens interface {
	Save(ctx context.Context, account, token string) error
	Load(ctx context.Context, account string) (string, error)
}

// Pulls reads pull requests from the providers.
type Pulls interface {
	ListPulls(ctx context.Context, token string, repos []model.Repo) service.ListResult
	PullDetail(ctx context.Context, token string, repo model.Repo, number int) (service.DetailResult, error)
	PullFiles(ctx context.Context, token string, repo model.Repo, number int) (service.FilesResult, error)
	PullTree(ctx context.Context, token string, repo model.Repo, number int) (service.TreeResult, error)
	Tree(ctx context.Context, token string, repo model.Repo, ref string) (service.TreeResult, error)
	File(ctx context.Context, token string, repo model.Repo, ref, path string) (service.FileResult, error)
	Archive(ctx context.Context, token string, repo model.Repo, ref string) (service.ArchiveResult, error)
}

// Deps are the collaborators of the handlers.
type Deps struct {
	Verifier      *auth.Verifier
	InternalToken string
	Backend       Backend
	Tokens        Tokens
	Pulls         Pulls
	// Health reports whether the service can serve requests.
	Health func(ctx context.Context) error
	Logger *slog.Logger
}

// Server implements the HTTP API.
type Server struct {
	Deps
}

// NewHandler builds the router with logging and panic recovery.
func NewHandler(deps Deps) http.Handler {
	s := &Server{Deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /api/v1/projects/{projectId}/pulls", s.authenticated(s.listPulls))
	mux.Handle("GET /api/v1/projects/{projectId}/repos/{repoId}/pulls", s.authenticated(s.listRepoPulls))
	mux.Handle("GET /api/v1/projects/{projectId}/repos/{repoId}/pulls/{number}", s.authenticated(s.publicDetail))
	mux.Handle("GET /api/v1/projects/{projectId}/repos/{repoId}/pulls/{number}/tree", s.authenticated(s.publicTree))
	mux.Handle("GET /api/v1/projects/{projectId}/repos/{repoId}/pulls/{number}/files", s.authenticated(s.publicFiles))
	mux.Handle("GET /internal/v1/projects/{projectId}/repos/{repoId}/pulls/{number}", s.internal(s.internalDetail))
	mux.Handle("GET /internal/v1/projects/{projectId}/repos/{repoId}/pulls/{number}/files", s.internal(s.internalFiles))
	mux.Handle("GET /internal/v1/projects/{projectId}/repos/{repoId}/tree", s.internal(s.internalTree))
	mux.Handle("GET /internal/v1/projects/{projectId}/repos/{repoId}/files", s.internal(s.internalFile))
	mux.Handle("GET /internal/v1/projects/{projectId}/repos/{repoId}/archive", s.internal(s.internalArchive))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.logRequests(s.recoverPanics(mux))
}

type listResponse struct {
	Pulls     []model.Pull        `json:"pulls"`
	Stale     bool                `json:"stale"`
	FetchedAt time.Time           `json:"fetchedAt"`
	Errors    []service.RepoError `json:"errors"`
}

type detailResponse struct {
	Pull      model.PullDetail `json:"pull"`
	Stale     bool             `json:"stale"`
	FetchedAt time.Time        `json:"fetchedAt"`
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Health(r.Context()); err != nil {
		s.Logger.Error("health check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, id identity) {
	projectID, ok := pathInt(r, "projectId")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown project")
		return
	}
	repos, ok := s.projectRepos(w, r, id, projectID)
	if !ok {
		return
	}
	s.writeList(w, r, id.providerToken, repos)
}

// listRepoPulls lists the pull requests of one repository of the project; only that repository
// is requested from the provider.
func (s *Server) listRepoPulls(w http.ResponseWriter, r *http.Request, id identity) {
	projectID, ok := pathInt(r, "projectId")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown project")
		return
	}
	repos, ok := s.projectRepos(w, r, id, projectID)
	if !ok {
		return
	}
	repo, ok := repoFromPath(w, r, repos)
	if !ok {
		return
	}
	s.writeList(w, r, id.providerToken, []model.Repo{repo})
}

func (s *Server) writeList(w http.ResponseWriter, r *http.Request, token string, repos []model.Repo) {
	res := s.Pulls.ListPulls(r.Context(), token, repos)
	writeJSON(w, http.StatusOK, listResponse{
		Pulls: res.Pulls, Stale: res.Stale, FetchedAt: res.FetchedAt, Errors: res.Errors,
	})
}

func (s *Server) publicDetail(w http.ResponseWriter, r *http.Request, id identity) {
	projectID, ok := pathInt(r, "projectId")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown project")
		return
	}
	repos, ok := s.projectRepos(w, r, id, projectID)
	if !ok {
		return
	}
	s.writeDetail(w, r, id.providerToken, repos)
}

func (s *Server) internalDetail(w http.ResponseWriter, r *http.Request) {
	token, repos, ok := s.internalContext(w, r)
	if !ok {
		return
	}
	s.writeDetail(w, r, token, repos)
}

// internalContext resolves the stored provider token of ?account= and the repositories of the
// project. Callers of the internal API are trusted services without a user JWT, so project
// membership of the account cannot be checked. It writes the error response and returns
// false on failure.
func (s *Server) internalContext(w http.ResponseWriter, r *http.Request) (string, []model.Repo, bool) {
	account := r.URL.Query().Get("account")
	if account == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query parameter account is required")
		return "", nil, false
	}
	projectID, ok := pathInt(r, "projectId")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown project")
		return "", nil, false
	}
	token, err := s.Tokens.Load(r.Context(), account)
	if errors.Is(err, store.ErrTokenNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no provider token stored for the account")
		return "", nil, false
	}
	if err != nil {
		s.Logger.Error("load provider token", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "cannot read the stored provider token")
		return "", nil, false
	}
	repos, err := s.Backend.Repos(r.Context(), "", projectID)
	if err != nil {
		s.writeBackendError(w, err)
		return "", nil, false
	}
	return token, repos, true
}

// projectRepos authorizes the caller for the project and returns its repositories.
// It writes the error response and returns false on failure.
func (s *Server) projectRepos(w http.ResponseWriter, r *http.Request, id identity, projectID int64) ([]model.Repo, bool) {
	member, err := s.Backend.IsMember(r.Context(), id.jwt, id.account, projectID)
	if err != nil {
		s.writeBackendError(w, err)
		return nil, false
	}
	if !member {
		writeError(w, http.StatusForbidden, "forbidden", "you are not a member of this project")
		return nil, false
	}
	repos, err := s.Backend.Repos(r.Context(), id.jwt, projectID)
	if err != nil {
		s.writeBackendError(w, err)
		return nil, false
	}
	return repos, true
}

func (s *Server) writeDetail(w http.ResponseWriter, r *http.Request, token string, repos []model.Repo) {
	repoID, okRepo := pathInt(r, "repoId")
	number, okNumber := pathInt(r, "number")
	repo, found := findRepo(repos, repoID)
	if !okRepo || !okNumber || !found {
		writeError(w, http.StatusNotFound, "not_found", "no such repository or pull request in this project")
		return
	}
	res, err := s.Pulls.PullDetail(r.Context(), token, repo, int(number))
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detailResponse{Pull: res.Pull, Stale: res.Stale, FetchedAt: res.FetchedAt})
}

func (s *Server) writeBackendError(w http.ResponseWriter, err error) {
	s.Logger.Error("pdd backend request failed", "error", err)
	writeError(w, http.StatusBadGateway, service.CodeUpstreamFailed, "the PDD backend is unavailable")
}

func (s *Server) writeProviderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "the requested resource does not exist or is not visible to you")
	case errors.Is(err, gitprovider.ErrQuotaReserve):
		writeError(w, http.StatusServiceUnavailable, service.CodeQuotaReserve, err.Error())
	case errors.Is(err, gitprovider.ErrUnauthorized):
		writeError(w, http.StatusBadGateway, service.CodeProviderUnauthorized, "the provider rejected your access token")
	default:
		s.Logger.Error("provider request failed", "error", err)
		writeError(w, http.StatusBadGateway, service.CodeUpstreamFailed, "the provider failed to answer: "+err.Error())
	}
}

// repoFromPath finds the repository named by {repoId}. It writes a 404 and returns false when
// the project has no such GitHub or GitLab repository.
func repoFromPath(w http.ResponseWriter, r *http.Request, repos []model.Repo) (model.Repo, bool) {
	repoID, ok := pathInt(r, "repoId")
	repo, found := findRepo(repos, repoID)
	if !ok || !found {
		writeError(w, http.StatusNotFound, "not_found", "no such repository in this project")
		return model.Repo{}, false
	}
	return repo, true
}

func findRepo(repos []model.Repo, id int64) (model.Repo, bool) {
	for _, repo := range repos {
		if repo.ID == id {
			return repo, true
		}
	}
	return model.Repo{}, false
}

func pathInt(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil && n >= 0
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
