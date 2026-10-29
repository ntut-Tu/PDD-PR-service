package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunFailsFastOnMissingToken(t *testing.T) {
	err := run(nil)

	if err == nil || !strings.Contains(err.Error(), "security.internalToken: required") {
		t.Fatalf("error = %v", err)
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

func TestRunReadsEveryConfigFileAndReportsInvalidValues(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.yaml")
	second := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(first, []byte("security:\n  internalToken: i\ngitGateway:\n  internalToken: g\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("limits:\n  maxConcurrent: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"--config", first, "--config", second})

	if err == nil || strings.Contains(err.Error(), "security.internalToken") || !strings.Contains(err.Error(), "limits.maxConcurrent") {
		t.Fatalf("both files must be merged, error = %v", err)
	}
}

func TestHelpFlagReturnsErrHelp(t *testing.T) {
	if err := run([]string{"-h"}); err == nil {
		t.Fatal("expected flag.ErrHelp from run")
	}
}

func TestRunServesUntilSignalled(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "v.yaml")
	yaml := "security: {internalToken: i}\ngitGateway: {internalToken: g}\nserver: {listenAddr: '127.0.0.1:0', shutdownTimeout: 1s}\n" +
		"network: {proxy: {listen: '127.0.0.1:0'}}\nstorage: {dataDir: " + filepath.Join(dir, "data") + "}\n" +
		"runtime: {containerd: {address: " + filepath.Join(dir, "no.sock") + "}}\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run([]string{"--config", cfg}) }()
	time.Sleep(300 * time.Millisecond)

	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
}
