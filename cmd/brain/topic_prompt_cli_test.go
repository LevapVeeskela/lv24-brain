package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newEngramCLI creates a local brain and returns its config path.
func newEngramCLI(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	return filepath.Join(dataDir, "brain.yaml"), dataDir
}

func TestNoteTopicCLIUpdatesInPlace(t *testing.T) {
	cfgPath, dataDir := newEngramCLI(t)
	topic := []string{"--topic", "architecture/auth", "--subtype", "architecture"}
	args := append([]string{"note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Auth model"}, topic...)

	code, stdout, stderr := runForTest(t, append(args, "--body", "first body")...)
	if code != 0 {
		t.Fatalf("first note exit code = %d (stderr: %s)", code, stderr)
	}
	firstID := strings.TrimSpace(strings.TrimPrefix(stdout, "note "))
	if firstID == "" {
		t.Fatalf("first note stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, append(args, "--body", "second body")...)
	if code != 0 {
		t.Fatalf("second note exit code = %d (stderr: %s)", code, stderr)
	}
	if strings.TrimSpace(strings.TrimPrefix(stdout, "note ")) != firstID {
		t.Fatalf("topic update wrote a new id: first %q, second %q", firstID, stdout)
	}

	code, stdout, stderr = runForTest(t, append(args, "--body", "second body")...)
	if code != 0 {
		t.Fatalf("idempotent note exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "(unchanged)") {
		t.Fatalf("idempotent output = %q, want (unchanged)", stdout)
	}

	matches, err := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "notes", "*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("notes files = %v, %v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## History", "first body", "second body", `"occurrences":2`, `"subtype":"architecture"`, `"topic_key":"architecture/auth"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("notes file missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"occurrences":3`) {
		t.Fatalf("idempotent write incremented occurrences:\n%s", raw)
	}

	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--topic", "architecture/auth", "auth")
	if code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Auth model") {
		t.Fatalf("search stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "record", "list", "--config", cfgPath, "--project", "demo", "--topic", "architecture/auth", "--json")
	if code != 0 {
		t.Fatalf("record list exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{`"topic_key": "architecture/auth"`, `"subtype": "architecture"`, `"occurrences": 2`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("record list JSON missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "record", "show", "--config", cfgPath, "--project", "demo", firstID, "--json")
	if code != 0 {
		t.Fatalf("record show exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{`"classification"`, `"topic_key": "architecture/auth"`, `"occurrences": 2`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("record show JSON missing %q:\n%s", want, stdout)
		}
	}
}

func TestPromptCLIIsOptIn(t *testing.T) {
	cfgPath, dataDir := newEngramCLI(t)
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Seed", "--body", "seed"); code != 0 {
		t.Fatalf("note exit code = %d (stderr: %s)", code, stderr)
	}
	code, _, stderr := runForTest(t, "prompt", "save", "--config", cfgPath, "--project", "demo", "--text", "secret prompt")
	if code != 1 {
		t.Fatalf("disabled prompt save exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "prompts.enabled") {
		t.Fatalf("disabled prompt save stderr = %q", stderr)
	}

	file, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\nprompts:\n  enabled: true\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runForTest(t, "prompt", "save", "--config", cfgPath, "--project", "demo", "--text", "secret prompt", "--session", "ses-7")
	if code != 0 {
		t.Fatalf("enabled prompt save exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.HasPrefix(stdout, "prompt demo ") {
		t.Fatalf("prompt save stdout = %q", stdout)
	}
	code, stdout, stderr = runForTest(t, "prompt", "list", "--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("prompt list exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "secret prompt") || !strings.Contains(stdout, "session=ses-7") {
		t.Fatalf("prompt list stdout = %q", stdout)
	}
	ignore, err := os.ReadFile(filepath.Join(dataDir, "projects", "demo", "prompts", ".gitignore"))
	if err != nil || strings.TrimSpace(string(ignore)) != "*" {
		t.Fatalf("prompt .gitignore = %q, %v", ignore, err)
	}
	if code, _, stderr := runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--json", "secret"); code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--json", "secret")
	if code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "secret prompt") {
		t.Fatalf("prompt text reached search:\n%s", stdout)
	}
	code, stdout, stderr = runForTest(t, "doctor", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("doctor exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "local-only") || !strings.Contains(stdout, "retention: 30 days") {
		t.Fatalf("doctor JSON missing prompt status:\n%s", stdout)
	}
}
