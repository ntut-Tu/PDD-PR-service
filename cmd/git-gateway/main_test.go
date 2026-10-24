package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunFailsFastOnMissingSecrets(t *testing.T) {
	err := run(nil)

	if err == nil {
		t.Fatal("run succeeded without secrets")
	}
	for _, path := range []string{"pdd.jwtSecret", "security.tokenKey", "security.internalToken"} {
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error does not name %s: %v", path, err)
		}
	}
}

func TestRunReportsUnreadableConfigFile(t *testing.T) {
	err := run([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml")})

	if err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	if err := run([]string{"--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestRunReadsEveryConfigFile(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.yaml")
	second := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(first, []byte("pdd:\n  jwtSecret: s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("security:\n  internalToken: i\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"--config", first, "--config", second})

	if err == nil || strings.Contains(err.Error(), "pdd.jwtSecret") || strings.Contains(err.Error(), "security.internalToken") ||
		!strings.Contains(err.Error(), "security.tokenKey") {
		t.Fatalf("both files must be merged, error = %v", err)
	}
}
