package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func runForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestVersionCommand(t *testing.T) {
	code, stdout, stderr := runForTest(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "brain "+version.Version) {
		t.Fatalf("stdout = %q, want version %q", stdout, version.Version)
	}
}

func TestHelpCommand(t *testing.T) {
	code, stdout, _ := runForTest(t, "help")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, command := range []string{"init", "doctor", "note", "sync", "serve", "update"} {
		if !strings.Contains(stdout, command) {
			t.Fatalf("help output missing command %q", command)
		}
	}
}

func TestNoArgumentsIsUsageError(t *testing.T) {
	code, _, stderr := runForTest(t)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q, want usage", stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := runForTest(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q, want unknown command", stderr)
	}
}

func TestStatusReadsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = "https://brain.example.com"
	cfg.User = "alex"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runForTest(t, "status", "--config", path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"mode: server", "server_url: https://brain.example.com", "user: alex", "engine_version: " + version.Version} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestUnimplementedCommandExitsNonZero(t *testing.T) {
	code, _, stderr := runForTest(t, "note", "--project", "demo", "--body", "hello")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not implemented") {
		t.Fatalf("stderr = %q, want not implemented", stderr)
	}
}

func TestInvalidFlagExitsTwo(t *testing.T) {
	code, _, _ := runForTest(t, "status", "--nope")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
