package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"time"

	"pdd/services/internal/gitgateway/auth"
)

// identity is the verified caller of a public request.
type identity struct {
	jwt           string
	account       string
	providerToken string
}

// authenticated verifies the PDD JWT, stores the provider token of the caller
// for later use without a JWT, and passes the identity on.
func (s *Server) authenticated(next func(http.ResponseWriter, *http.Request, identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt, ok := auth.BearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		claims, err := s.Verifier.Verify(jwt)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		if err := s.Tokens.Save(r.Context(), claims.Account, claims.ProviderToken); err != nil {
			s.Logger.Error("persist provider token", "error", err)
		}
		next(w, r, identity{jwt: jwt, account: claims.Account, providerToken: claims.ProviderToken})
	})
}

// internal requires the shared X-Internal-Token secret.
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

// logRequests logs method, path, status and duration. Query strings and
// headers are left out because they can carry secrets.
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
