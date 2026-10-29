package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"pdd/services/internal/gitgateway/provider"
	"pdd/services/internal/gitgateway/service"
)

const archiveSHA = "0123456789abcdef0123456789abcdef01234567"

var internalArchive = "/internal/v1/projects/7/repos/12/archive?account=alice&ref=" + archiveSHA

func TestInternalArchiveStreamsTheGzip(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	f.pulls.archive = "gzip-bytes"

	rec := f.do(http.MethodGet, internalArchive, internalHeader(internalToken))

	assertOK(t, rec)
	if rec.Body.String() != "gzip-bytes" || rec.Header().Get("Content-Type") != "application/gzip" || rec.Header().Get("Content-Length") != "10" {
		t.Errorf("body %q headers %v", rec.Body, rec.Header())
	}
	if f.pulls.gotToken != "gho_alice" || f.pulls.gotRepo.ID != 12 || f.pulls.gotRef != archiveSHA || f.backend.reposJWT != "" {
		t.Errorf("token %q repo %+v ref %q jwt %q", f.pulls.gotToken, f.pulls.gotRepo, f.pulls.gotRef, f.backend.reposJWT)
	}
}

func TestInternalArchiveOfAGitLabRepository(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)

	assertOK(t, f.do(http.MethodGet, "/internal/v1/projects/7/repos/13/archive?account=alice&ref="+archiveSHA, internalHeader(internalToken)))

	if f.pulls.gotRepo.ID != 13 {
		t.Errorf("repo = %+v", f.pulls.gotRepo)
	}
}

func TestInternalArchiveNormalizesTheRefToLowerCase(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)

	assertOK(t, f.do(http.MethodGet, strings.Replace(internalArchive, archiveSHA, strings.ToUpper(archiveSHA), 1), internalHeader(internalToken)))

	if f.pulls.gotRef != archiveSHA {
		t.Errorf("ref = %q", f.pulls.gotRef)
	}
}

func TestInternalArchiveRejectsABadRef(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	base := "/internal/v1/projects/7/repos/12/archive?account=alice"

	for name, query := range map[string]string{
		"missing":       "",
		"branch name":   "&ref=main",
		"short sha":     "&ref=" + archiveSHA[:39],
		"long sha":      "&ref=" + archiveSHA + "0",
		"not hex":       "&ref=" + strings.Repeat("g", 40),
		"traversal":     "&ref=" + strings.Repeat("a", 30) + "%2F..%2F..%2Fxyz",
		"whitespace":    "&ref=%20" + archiveSHA[:39],
		"trailing line": "&ref=" + archiveSHA + "%0A",
	} {
		t.Run(name, func(t *testing.T) {
			assertError(t, f.do(http.MethodGet, base+query, internalHeader(internalToken)), http.StatusBadRequest, "bad_request")
		})
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called for a rejected ref")
	}
}

func TestInternalArchiveAccessControl(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)

	for _, header := range []http.Header{nil, internalHeader("nope"), validAuth()} {
		assertError(t, f.do(http.MethodGet, internalArchive, header), http.StatusUnauthorized, "unauthorized")
	}
	h := internalHeader(internalToken)
	tests := []struct {
		name, path string
		status     int
		code       string
	}{
		{"without account", "/internal/v1/projects/7/repos/12/archive?ref=" + archiveSHA, http.StatusBadRequest, "bad_request"},
		{"unknown account", "/internal/v1/projects/7/repos/12/archive?account=bob&ref=" + archiveSHA, http.StatusNotFound, "not_found"},
		{"unknown repo", "/internal/v1/projects/7/repos/99/archive?account=alice&ref=" + archiveSHA, http.StatusNotFound, "not_found"},
		{"bad repo id", "/internal/v1/projects/7/repos/x/archive?account=alice&ref=" + archiveSHA, http.StatusNotFound, "not_found"},
		{"bad project", "/internal/v1/projects/x/repos/12/archive?account=alice&ref=" + archiveSHA, http.StatusNotFound, "not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { assertError(t, f.do(http.MethodGet, tt.path, h), tt.status, tt.code) })
	}
	if f.pulls.gotToken != "" {
		t.Error("provider called for a rejected request")
	}
}

func TestInternalArchiveIsNotAPublicEndpoint(t *testing.T) {
	f := newFixture(t)

	assertError(t, f.do(http.MethodGet, "/api/v1/projects/7/repos/12/archive?ref="+archiveSHA, validAuth()), http.StatusNotFound, "not_found")
}

func TestInternalArchiveErrors(t *testing.T) {
	f := newFixture(t)
	storedAlice(f)
	h := internalHeader(internalToken)

	for name, tt := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"not found":      {provider.ErrNotFound, http.StatusNotFound, "not_found"},
		"too large":      {fmt.Errorf("wrapped: %w", service.ErrArchiveTooLarge), http.StatusRequestEntityTooLarge, "archive_too_large"},
		"provider error": {&statusErr{}, http.StatusBadGateway, "upstream_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			f.pulls.archiveErr = tt.err
			assertError(t, f.do(http.MethodGet, internalArchive, h), tt.status, tt.code)
		})
	}
}
