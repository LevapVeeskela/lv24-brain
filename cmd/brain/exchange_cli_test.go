package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestExchangeCLIExportImport(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	runForTestOK := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := runForTest(t, args...)
		if code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, stderr)
		}
		return stdout
	}
	runForTestOK("init", "--mode", "local", "--data-dir", dataDir)
	runForTestOK("note", "--create", "--config", cfg, "--project", "demo", "--title", "n", "--body", "b")

	outDir := filepath.Join(t.TempDir(), "export")
	code, stdout, stderr := runForTest(t, "export", "--config", cfg, "--out", outDir, "--json")
	if code != 0 {
		t.Fatalf("export exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "\"records\":") {
		t.Fatalf("export json = %q", stdout)
	}

	// import into fresh data dir
	dataDir2 := filepath.Join(t.TempDir(), "brain2")
	cfg2 := filepath.Join(dataDir2, "brain.yaml")
	runForTestOK("init", "--mode", "local", "--data-dir", dataDir2)

	code, stdout, stderr = runForTest(t, "import", "--config", cfg2, "--from", outDir, "--create-projects", "--json")
	if code != 0 {
		t.Fatalf("import exit %d: %s", code, stderr)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &report); err != nil {
		t.Fatalf("import json: %v\n%s", err, stdout)
	}
	if report["created"].(float64) != 2 {
		t.Fatalf("created = %v", report["created"])
	}

	// second import: unchanged, exit 0
	code, _, stderr = runForTest(t, "import", "--config", cfg2, "--from", outDir, "--create-projects")
	if code != 0 {
		t.Fatalf("idempotent import exit %d: %s", code, stderr)
	}

	// rollback rejected
	code, _, stderr = runForTest(t, "import", "--config", cfg2, "--from", outDir, "--rollback", "x")
	if code != 2 || !strings.Contains(stderr, "not supported") {
		t.Fatalf("rollback exit=%d stderr=%q", code, stderr)
	}

	// missing --from
	code, _, _ = runForTest(t, "import", "--config", cfg2)
	if code != 2 {
		t.Fatalf("missing from exit=%d", code)
	}

	// missing --out
	code, _, _ = runForTest(t, "export", "--config", cfg)
	if code != 2 {
		t.Fatalf("missing out exit=%d", code)
	}
}

func TestExchangeCLIZipAndDryRun(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	runForTestOK := func(args ...string) string {
		t.Helper()
		code, stdout, stderr := runForTest(t, args...)
		if code != 0 {
			t.Fatalf("%v exit %d: %s", args, code, stderr)
		}
		return stdout
	}
	runForTestOK("init", "--mode", "local", "--data-dir", dataDir)
	runForTestOK("note", "--create", "--config", cfg, "--project", "demo", "--title", "n", "--body", "b")

	zipPath := filepath.Join(t.TempDir(), "bundle.zip")
	runForTestOK("export", "--config", cfg, "--out", zipPath)

	dataDir2 := filepath.Join(t.TempDir(), "brain2")
	cfg2 := filepath.Join(dataDir2, "brain.yaml")
	runForTestOK("init", "--mode", "local", "--data-dir", dataDir2)

	// dry run writes nothing
	code, stdout, stderr := runForTest(t, "import", "--config", cfg2, "--from", zipPath, "--create-projects", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("dryrun exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "\"created\":2") {
		t.Fatalf("dryrun json = %q", stdout)
	}
	// verify nothing written
	code, stdout, _ = runForTest(t, "record", "list", "demo", "--config", cfg2)
	_ = code
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("dry run wrote records: %q", stdout)
	}
}
