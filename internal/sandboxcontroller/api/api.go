// Package api exposes the internal HTTP API of the sandbox-controller: create, read and delete
// SandboxRuns, plus the health check. Every /internal endpoint needs the shared token.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"pdd/services/internal/sandboxcontroller/model"
	"pdd/services/internal/sandboxcontroller/store"
)

const maxBodyBytes = 8 << 20

// Runs is the record store the handlers use.
type Runs interface {
	Create(ctx context.Context, run model.Run) error
	Get(ctx context.Context, id string) (model.Run, error)
	RequestDeletion(ctx context.Context, id string) error
	Ping(ctx context.Context) error
}

// Deps are the collaborators of the handlers.
type Deps struct {
	InternalToken string
	Runs          Runs
	// Trigger asks the reconcile loop for a pass.
	Trigger func()
	// SandboxAvailable reports whether runsc is registered.
	SandboxAvailable func(ctx context.Context) bool
	Logger           *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Server implements the HTTP API.
type Server struct{ Deps }

// NewHandler builds the router with logging and panic recovery.
func NewHandler(deps Deps) http.Handler {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	s := &Server{Deps: deps}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("POST /internal/v1/runs", s.internal(s.createRun))
	mux.Handle("GET /internal/v1/runs/{id}", s.internal(s.getRun))
	mux.Handle("DELETE /internal/v1/runs/{id}", s.internal(s.deleteRun))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	return s.logRequests(s.recoverPanics(mux))
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Runs.Ping(r.Context()); err != nil {
		s.Logger.Error("health check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "service is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"runsc": s.SandboxAvailable(r.Context())})
}

type createRequest struct {
	Spec model.Spec `json:"spec"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be JSON of the form {\"spec\": {...}}: "+err.Error())
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "unexpected data after the JSON body")
		return
	}
	if err := req.Spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Spec.Files == nil {
		req.Spec.Files = []model.File{}
	}
	run := model.Run{
		ID:   model.NewID(),
		Spec: req.Spec,
		Status: model.Status{
			Phase: model.Pending, Message: "waiting for a free sandbox",
			Conditions: []model.Condition{}, CreatedAt: s.Now().UTC(),
		},
	}
	if err := s.Runs.Create(r.Context(), run); err != nil {
		s.Logger.Error("create run", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "cannot store the run")
		return
	}
	s.Trigger()
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	if err := s.Runs.RequestDeletion(r.Context(), run.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Logger.Error("request deletion", "run", run.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "cannot mark the run for deletion")
		return
	}
	s.Trigger()
	run.DeletionRequested = true
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) find(w http.ResponseWriter, r *http.Request) (model.Run, bool) {
	id := r.PathValue("id")
	if !model.ValidID(id) {
		writeError(w, http.StatusNotFound, "not_found", "no such run")
		return model.Run{}, false
	}
	run, err := s.Runs.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such run")
		return model.Run{}, false
	}
	if err != nil {
		s.Logger.Error("read run", "run", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "cannot read the run")
		return model.Run{}, false
	}
	return run, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// internal requires the shared X-Internal-Token secret, compared in constant time.
func (s *Server) internal(next http.HandlerFunc) http.Handler {
	expected := sha256.Sum256([]byte(s.InternalToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		given := sha256.Sum256([]byte(r.Header.Get("X-Internal-Token")))
		if subtle.ConstantTimeCompare(expected[:], given[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid internal token")
			return
		}
		next(w, r)
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Logger.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logRequests logs method, path, status and duration. Query strings and headers are left out
// because they can carry secrets.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.Logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
