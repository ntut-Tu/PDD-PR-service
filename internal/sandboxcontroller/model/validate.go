package model

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Limits of a run request.
const (
	MaxFiles        = 200
	MaxContentBytes = 4 << 20
	MaxTestClasses  = 200
	maxPathLen      = 512
	maxAccountLen   = 128
)

var (
	shaPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	classPattern = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)
	idPattern    = regexp.MustCompile(`^run_[0-9a-f]{24}$`)
)

// ValidID reports whether s has the shape of a run id.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// CleanTestPath returns the normalized relative path of a generated file, which must lie under
// a src/test directory (of the repository root or of a module) and never leave the workspace.
func CleanTestPath(p string) (string, error) {
	if p == "" || len(p) > maxPathLen || strings.ContainsAny(p, "\x00\\") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("path %q must be relative", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("path %q must not contain ..", p)
		}
	}
	clean := path.Clean(p)
	segs := strings.Split(clean, "/")
	for i := 0; i+1 < len(segs)-1; i++ {
		if segs[i] == "src" && segs[i+1] == "test" {
			return clean, nil
		}
	}
	return "", fmt.Errorf("path %q is outside src/test", p)
}

// Validate checks a spec received over the API.
func (s *Spec) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if a := strings.TrimSpace(s.Account); a == "" || len(a) > maxAccountLen || strings.ContainsFunc(a, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		add("account: must be 1-%d printable characters", maxAccountLen)
	}
	if s.ProjectID < 1 {
		add("projectId: must be positive")
	}
	if s.RepoID < 1 {
		add("repoId: must be positive")
	}
	if !shaPattern.MatchString(s.HeadSHA) {
		add("headSha: must be 40 lowercase hex characters")
	}
	if len(s.Files) > MaxFiles {
		add("files: at most %d files", MaxFiles)
	}
	total := 0
	for i := range s.Files {
		clean, err := CleanTestPath(s.Files[i].Path)
		if err != nil {
			add("files[%d].path: %v", i, err)
		}
		s.Files[i].Path = clean
		total += len(s.Files[i].Content)
	}
	if total > MaxContentBytes {
		add("files: content is larger than %d bytes", MaxContentBytes)
	}
	if len(s.TestClasses) == 0 || len(s.TestClasses) > MaxTestClasses {
		add("testClasses: between 1 and %d classes required", MaxTestClasses)
	}
	for i, c := range s.TestClasses {
		if !classPattern.MatchString(c) {
			add("testClasses[%d]: %q is not a fully qualified class name", i, c)
		}
	}
	return errors.Join(errs...)
}
