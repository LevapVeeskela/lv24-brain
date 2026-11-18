package agentcontrol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeHookTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHookStatusReportsInstalledTrustAndLastRun(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	writeHookTestFile(t, filepath.Join(home, ".claude", "settings.json"),
		`{"hooks":{"SessionStart":[{"hooks":[{"command":"brain session start --hook claude"}]}],`+
			`"SessionEnd":[{"hooks":[{"command":"brain session end --hook claude"}]}]}}`)
	codexPath := filepath.Join(home, ".codex", "hooks.json")
	writeHookTestFile(t, codexPath,
		`{"hooks":{"SessionStart":[{"hooks":[{"command":"brain session start --hook codex"}]}],`+
			`"Stop":[{"hooks":[{"command":"brain session end --hook codex"}]}],`+
			`"SessionEnd":[{"hooks":[{"command":"brain session end --hook codex"}]}]}}`)
	writeHookTestFile(t, filepath.Join(home, ".config", "opencode", "plugins", "brain-session-end.js"),
		`export const brain = { session: "end" }`)

	baseline := hookFileHash(codexPath, "codex")
	if baseline == "" {
		t.Fatal("codex baseline hash is empty")
	}
	trust, err := json.Marshal(map[string]any{
		"version": 1,
		"hosts":   map[string]any{"codex": map[string]string{"hash": baseline}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeHookTestFile(t, filepath.Join(dataDir, "index", hookTrustFileName), string(trust))
	writeHookTestFile(t, filepath.Join(dataDir, "index", hookLogFileName),
		`{"time":"2026-11-15T10:00:00Z","host":"codex","event":"Stop","exit":0}`+"\n")

	statuses, err := newHookStatusService(home, dataDir).Hooks(context.Background())
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("hosts = %+v", statuses)
	}
	claude, codex, opencode := statuses[0], statuses[1], statuses[2]
	if claude.Host != "claude" || !claude.Installed || claude.Detail != "ok" || claude.Trusted != nil {
		t.Fatalf("claude = %+v", claude)
	}
	if codex.Host != "codex" || !codex.Installed || codex.Trusted == nil || !*codex.Trusted ||
		codex.Trust != "approved" || codex.TrustDrift {
		t.Fatalf("codex = %+v", codex)
	}
	if codex.LastRun == nil || *codex.LastRun != "2026-11-15T10:00:00Z" || codex.LastEvent != "Stop" {
		t.Fatalf("codex last run = %+v", codex)
	}
	if opencode.Host != "opencode" || !opencode.Installed {
		t.Fatalf("opencode = %+v", opencode)
	}

	// A changed command invalidates the stored baseline.
	writeHookTestFile(t, codexPath,
		`{"hooks":{"SessionStart":[{"hooks":[{"command":"brain --other session start --hook codex"}]}]}}`)
	statuses, err = newHookStatusService(home, dataDir).Hooks(context.Background())
	if err != nil {
		t.Fatalf("Hooks after drift: %v", err)
	}
	codex = statuses[1]
	if codex.Trusted == nil || *codex.Trusted || codex.Trust != "drift" || !codex.TrustDrift || codex.Approve == "" {
		t.Fatalf("drifted codex = %+v", codex)
	}
	if codex.Detail == "ok" {
		t.Fatalf("incomplete codex hooks reported as ok: %+v", codex)
	}
}

func TestHookStatusMissingHostsAreNotInstalled(t *testing.T) {
	statuses, err := newHookStatusService(t.TempDir(), t.TempDir()).Hooks(context.Background())
	if err != nil {
		t.Fatalf("Hooks: %v", err)
	}
	for _, status := range statuses {
		if status.Installed || status.Detail == "" {
			t.Fatalf("missing host %s = %+v", status.Host, status)
		}
	}
}
