package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// linksCLIFixture is a local instance where a note links to a decision (plain
// and with display text) and to a missing record.
type linksCLIFixture struct {
	cfg, decision, note string
}

func newLinksCLIFixture(t *testing.T) linksCLIFixture {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	decision := strings.Fields(recordCLIOK(t, "decision", "--create", "--config", cfg, "--project", "demo",
		"--title", "Use fsstore", "--status", "accepted"))[1]
	note := strings.Fields(recordCLIOK(t, "note", "--config", cfg, "--project", "demo", "--title", "Why",
		"--body", "per [["+decision+"|the store decision]] and [[decision-gone]]"))[1]
	return linksCLIFixture{cfg: cfg, decision: decision, note: note}
}

func TestRecordShowLinksSections(t *testing.T) {
	// Arrange
	fx := newLinksCLIFixture(t)

	// Act
	noteText := recordCLIOK(t, "record", "show", fx.note, "--project", "demo", "--config", fx.cfg)
	decisionText := recordCLIOK(t, "record", "show", fx.decision, "--project", "demo", "--config", fx.cfg)
	data := recordJSONData(t, recordCLIOK(t, "record", "show", fx.note, "--project", "demo", "--json", "--config", fx.cfg)).(map[string]any)

	// Assert
	for _, want := range []string{
		"Links:\n  - " + fx.decision + " (the store decision) -> decision [accepted] Use fsstore (in body)",
		"  - decision-gone -> missing (record decision-gone not found in project demo) (in body)",
		"Backlinks:\n  (none)",
	} {
		if !strings.Contains(noteText, want) {
			t.Fatalf("note show missing %q in:\n%s", want, noteText)
		}
	}
	if !strings.Contains(decisionText, "Links:\n  (none)\nBacklinks:\n  - note "+fx.note+" Why (in body)") {
		t.Fatalf("decision show:\n%s", decisionText)
	}
	record := data["record"].(map[string]any)
	links := record["links"].([]any)
	if len(links) != 2 {
		t.Fatalf("json links = %v", links)
	}
	first := links[0].(map[string]any)
	if first["state"] != "found" || first["kind"] != "decision" || first["status"] != "accepted" || first["text"] != "the store decision" {
		t.Fatalf("json link = %v", first)
	}
	if links[1].(map[string]any)["state"] != "missing" {
		t.Fatalf("json missing link = %v", links[1])
	}
	if backlinks, ok := record["backlinks"].([]any); !ok || len(backlinks) != 0 {
		t.Fatalf("json backlinks = %v, want []", record["backlinks"])
	}
}

func TestLinksCommand(t *testing.T) {
	// Arrange
	fx := newLinksCLIFixture(t)

	// Act
	text := recordCLIOK(t, "links", fx.decision, "--project", "demo", "--config", fx.cfg)
	data := recordJSONData(t, recordCLIOK(t, "links", fx.decision, "--project", "demo", "--json", "--config", fx.cfg)).(map[string]any)
	broken := recordCLIOK(t, "links", "--broken", "--project", "demo", "--config", fx.cfg)
	brokenData := recordJSONData(t, recordCLIOK(t, "links", "--broken", "--project", "demo", "--json", "--config", fx.cfg)).(map[string]any)

	// Assert
	if !strings.Contains(text, fx.decision) || !strings.Contains(text, "Backlinks:\n  - note "+fx.note) {
		t.Fatalf("links text:\n%s", text)
	}
	if data["record"].(map[string]any)["id"] != fx.decision || len(data["backlinks"].([]any)) != 1 || len(data["links"].([]any)) != 0 {
		t.Fatalf("links json = %v", data)
	}
	if !strings.Contains(broken, "note "+fx.note+" -> [[decision-gone]] in body") || !strings.Contains(broken, "1 broken links in demo") {
		t.Fatalf("broken text:\n%s", broken)
	}
	items := brokenData["broken"].([]any)
	if brokenData["project_id"] != "demo" || len(items) != 1 {
		t.Fatalf("broken json = %v", brokenData)
	}
	if link := items[0].(map[string]any)["link"].(map[string]any); link["ref"] != "decision-gone" || link["state"] != "missing" {
		t.Fatalf("broken json link = %v", link)
	}
}

func TestLinksCommandUsageErrors(t *testing.T) {
	fx := newLinksCLIFixture(t)
	if code, _, _ := recordCLI(t, "links", "--config", fx.cfg); code != 2 {
		t.Fatalf("links without id = exit %d, want 2", code)
	}
	if code, _, _ := recordCLI(t, "links", fx.note, "--broken", "--config", fx.cfg); code != 2 {
		t.Fatalf("links <id> --broken = exit %d, want 2", code)
	}
	if code, _, stderr := recordCLI(t, "links", "nope", "--project", "demo", "--config", fx.cfg); code == 0 || stderr == "" {
		t.Fatalf("links unknown id = exit %d", code)
	}
	if code, _, stderr := recordCLI(t, "links", "--help"); code != 0 || !strings.Contains(stderr, "[[<project>/<id>]]") {
		t.Fatalf("links --help = exit %d: %s", code, stderr)
	}
}

func TestMCPBrainRecordLinks(t *testing.T) {
	// Arrange
	fx := newLinksCLIFixture(t)

	// Act
	code, responses, stderr := runMCPForTest(t, fx.cfg,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		mcpToolCall(2, "brain_record", `{"id":"`+fx.decision+`","project":"demo"}`),
	)

	// Assert
	if code != 0 || len(responses) != 2 {
		t.Fatalf("runMCP exit %d, responses %d (stderr: %s)", code, len(responses), stderr)
	}
	result := mcpResult(t, responses[1])
	structured := mcpStructured(t, result)
	backlinks, ok := structured["backlinks"].([]any)
	if !ok || len(backlinks) != 1 || backlinks[0].(map[string]any)["id"] != fx.note {
		t.Fatalf("brain_record backlinks = %v", structured["backlinks"])
	}
	if links, ok := structured["links"].([]any); !ok || len(links) != 0 {
		t.Fatalf("brain_record links = %v, want []", structured["links"])
	}
	if text := result["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(text, "backlink: note "+fx.note) {
		t.Fatalf("brain_record text = %q", text)
	}
}
