package api

import (
	"net/http"
	"time"

	"pdd/services/internal/gitgateway/model"
	"pdd/services/internal/gitgateway/service"
)

type treeResponse struct {
	Ref       string            `json:"ref"`
	Entries   []model.TreeEntry `json:"entries"`
	Truncated bool              `json:"truncated"`
	Stale     bool              `json:"stale"`
	FetchedAt time.Time         `json:"fetchedAt"`
}

type filesResponse struct {
	HeadSHA   string              `json:"headSha"`
	BaseSHA   string              `json:"baseSha"`
	Files     []model.ChangedFile `json:"files"`
	Truncated bool                `json:"truncated"`
}

// publicFilesResponse is the internal files body plus the freshness fields of the public API.
type publicFilesResponse struct {
	filesResponse
	Stale     bool      `json:"stale"`
	FetchedAt time.Time `json:"fetchedAt"`
}

type fileResponse struct {
	Ref       string `json:"ref"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
}

// publicTree serves the repository tree at the head commit of a pull request.
func (s *Server) publicTree(w http.ResponseWriter, r *http.Request, id identity) {
	projectID, ok := pathInt(r, "projectId")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown project")
		return
	}
	repos, ok := s.projectRepos(w, r, id, projectID)
	if !ok {
		return
	}
	repo, number, ok := repoAndNumber(w, r, repos)
	if !ok {
		return
	}
	res, err := s.Pulls.PullTree(r.Context(), id.providerToken, repo, number)
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeTree(w, res)
}

// publicFiles serves the files changed by a pull request with the caller's own provider token.
func (s *Server) publicFiles(w http.ResponseWriter, r *http.Request, id identity) {
	projectID, ok := pathInt(r, "projectId")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown project")
		return
	}
	repos, ok := s.projectRepos(w, r, id, projectID)
	if !ok {
		return
	}
	repo, number, ok := repoAndNumber(w, r, repos)
	if !ok {
		return
	}
	res, err := s.Pulls.PullFiles(r.Context(), id.providerToken, repo, number)
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, publicFilesResponse{filesResponse: newFilesResponse(res), Stale: res.Stale, FetchedAt: res.FetchedAt})
}

func (s *Server) internalFiles(w http.ResponseWriter, r *http.Request) {
	token, repos, ok := s.internalContext(w, r)
	if !ok {
		return
	}
	repo, number, ok := repoAndNumber(w, r, repos)
	if !ok {
		return
	}
	res, err := s.Pulls.PullFiles(r.Context(), token, repo, number)
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newFilesResponse(res))
}

func newFilesResponse(res service.FilesResult) filesResponse {
	files := res.Files
	if files == nil {
		files = []model.ChangedFile{}
	}
	return filesResponse{HeadSHA: res.HeadSHA, BaseSHA: res.BaseSHA, Files: files, Truncated: res.Truncated}
}

func (s *Server) internalTree(w http.ResponseWriter, r *http.Request) {
	token, repos, ok := s.internalContext(w, r)
	if !ok {
		return
	}
	repo, ok := repoFromPath(w, r, repos)
	if !ok {
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query parameter ref is required")
		return
	}
	res, err := s.Pulls.Tree(r.Context(), token, repo, ref)
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeTree(w, res)
}

func (s *Server) internalFile(w http.ResponseWriter, r *http.Request) {
	token, repos, ok := s.internalContext(w, r)
	if !ok {
		return
	}
	repo, ok := repoFromPath(w, r, repos)
	if !ok {
		return
	}
	ref, path := r.URL.Query().Get("ref"), r.URL.Query().Get("path")
	if ref == "" || path == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "query parameters ref and path are required")
		return
	}
	res, err := s.Pulls.File(r.Context(), token, repo, ref, path)
	if err != nil {
		s.writeProviderError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileResponse{
		Ref: res.Ref, Path: res.Path, Size: res.Size, Content: res.Content, Binary: res.Binary, Truncated: res.Truncated,
	})
}

func writeTree(w http.ResponseWriter, res service.TreeResult) {
	entries := res.Entries
	if entries == nil {
		entries = []model.TreeEntry{}
	}
	writeJSON(w, http.StatusOK, treeResponse{
		Ref: res.Ref, Entries: entries, Truncated: res.Truncated, Stale: res.Stale, FetchedAt: res.FetchedAt,
	})
}

// repoAndNumber resolves {repoId} and {number}. It writes a 404 and returns false when either
// is unknown.
func repoAndNumber(w http.ResponseWriter, r *http.Request, repos []model.Repo) (model.Repo, int, bool) {
	repo, ok := repoFromPath(w, r, repos)
	if !ok {
		return model.Repo{}, 0, false
	}
	number, ok := pathInt(r, "number")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such pull request")
		return model.Repo{}, 0, false
	}
	return repo, int(number), true
}
