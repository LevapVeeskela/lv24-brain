package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestAppendPromptWritesLocalOnlyFile(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	ctx := context.Background()
	before, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision before: %v", err)
	}
	at := time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)
	if err := store.AppendPrompt(ctx, domain.Prompt{ProjectID: "lv24-brain", At: at, Session: "ses-1", Text: "first prompt"}); err != nil {
		t.Fatalf("AppendPrompt: %v", err)
	}
	if err := store.AppendPrompt(ctx, domain.Prompt{ProjectID: "lv24-brain", At: at.Add(time.Minute), Text: "second prompt"}); err != nil {
		t.Fatalf("AppendPrompt second: %v", err)
	}
	dir := filepath.Join(store.root, "projects", "lv24-brain", "prompts")
	files := globFiles(t, filepath.Join(dir, "*.md"))
	if len(files) != 1 {
		t.Fatalf("prompt files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "visibility: local") {
		t.Fatalf("prompt file is not marked local:\n%s", raw)
	}
	ignore, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || strings.TrimSpace(string(ignore)) != "*" {
		t.Fatalf("prompt .gitignore = %q, %v", ignore, err)
	}
	after, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision after: %v", err)
	}
	if after.Value != before.Value {
		t.Fatalf("a prompt changed the data revision: %s -> %s", before.Value, after.Value)
	}
	notes, err := store.ListNotes(ctx, "lv24-brain")
	if err != nil || len(notes) != 0 {
		t.Fatalf("prompts leaked into notes: %d, %v", len(notes), err)
	}

	prompts, err := store.ListPrompts(ctx, "lv24-brain", 10)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d", len(prompts))
	}
	if prompts[0].Text != "second prompt" || prompts[1].Text != "first prompt" {
		t.Fatalf("prompt order = %q, %q", prompts[0].Text, prompts[1].Text)
	}
	if prompts[1].Session != "ses-1" || prompts[0].Session != "" {
		t.Fatalf("sessions = %q, %q", prompts[1].Session, prompts[0].Session)
	}
	if prompts[0].Path == "" || strings.Contains(prompts[0].Path, "\\") {
		t.Fatalf("prompt path = %q", prompts[0].Path)
	}
}

func TestPromptGitignoreIsRepaired(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	ctx := context.Background()
	dir := filepath.Join(store.root, "projects", "lv24-brain", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendPrompt(ctx, domain.Prompt{ProjectID: "lv24-brain", Text: "prompt"}); err != nil {
		t.Fatalf("AppendPrompt: %v", err)
	}
	ignore, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil || strings.TrimSpace(string(ignore)) != "*" {
		t.Fatalf("prompt .gitignore = %q, %v", ignore, err)
	}
}

func TestListPromptsWithoutZoneIsEmpty(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	prompts, err := store.ListPrompts(context.Background(), "lv24-brain", 0)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 0 {
		t.Fatalf("prompts = %d", len(prompts))
	}
}
