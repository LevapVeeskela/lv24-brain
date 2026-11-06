package journal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestAppendAndReadAcrossUsersNewestFirst(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()
	base := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	for i, entry := range []ports.JournalEntry{
		{Time: base, Event: "note", User: "alice", Project: "p1", ID: "n1", Title: "first"},
		{Time: base.Add(2 * time.Minute), Event: "commit", User: "bob", Project: "p1", Fields: map[string]string{"head": "abc123", "ts": "spoofed"}},
		{Time: base.Add(time.Minute), Event: "decision", User: "alice", Project: "p2", ID: "d1"},
	} {
		if err := store.Append(ctx, entry); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	for _, name := range []string{"alice.jsonl", "bob.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, Dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	all, err := store.Read(ctx, ports.JournalQuery{})
	if err != nil || len(all) != 3 {
		t.Fatalf("read all = %+v, %v", all, err)
	}
	if all[0].Event != "commit" || all[0].Fields["head"] != "abc123" || !all[0].Time.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("newest = %+v (reserved key must not be overridden)", all[0])
	}
	p1, _ := store.Read(ctx, ports.JournalQuery{Project: "p1"})
	if len(p1) != 2 {
		t.Fatalf("project filter = %+v", p1)
	}
	alice, _ := store.Read(ctx, ports.JournalQuery{User: "ALICE", Limit: 1})
	if len(alice) != 1 || alice[0].ID != "d1" {
		t.Fatalf("user filter + limit = %+v", alice)
	}
}

func TestReadsLegacyLinesWithBOMAndSkipsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "\xef\xbb\xbf" + `{"ts":"2026-11-05T02:21:48.8212254+03:00","event":"commit","project":"lv24","branch":"develop","head":"fb09052ea"}` + "\n" +
		"not json\n" +
		`{"ts":"2026-11-05T02:52:15.6467900+03:00","event":"github_sync","repo":"a/b","project_id":"lv24-harvest","issues":0}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, Dir, "Veeskela.jsonl"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := New(dir).Read(context.Background(), ports.JournalQuery{})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	sync := entries[0]
	if sync.Event != "github_sync" || sync.Project != "lv24-harvest" || sync.User != "Veeskela" || sync.Fields["issues"] != "0" {
		t.Fatalf("legacy sync line = %+v", sync)
	}
	if commit := entries[1]; commit.Fields["branch"] != "develop" || commit.Project != "lv24" {
		t.Fatalf("legacy commit line = %+v", commit)
	}
}

func TestFileNameCannotEscapeDirectory(t *testing.T) {
	for user, want := range map[string]string{
		"alice":          "alice.jsonl",
		"../../etc/x":    "etc-x.jsonl",
		`C:\Users\bob`:   "C--Users-bob.jsonl",
		"":               "unknown.jsonl",
		"Павел":          "unknown.jsonl",
		"me@example.com": "me-example.com.jsonl",
	} {
		if got := FileName(user); got != want {
			t.Errorf("FileName(%q) = %q, want %q", user, got, want)
		}
		if strings.ContainsAny(FileName(user), `/\`) {
			t.Errorf("FileName(%q) contains a separator", user)
		}
	}
}

func TestAppendBoundsLineSize(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	huge := strings.Repeat("x", 100000)
	fields := map[string]string{}
	for i := 0; i < 50; i++ {
		fields[string(rune('a'+i%26))+strings.Repeat("k", i)] = huge
	}
	if err := store.Append(context.Background(), ports.JournalEntry{Event: "note", User: "u", Title: huge, Fields: fields}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, Dir, "u.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxLineBytes+1 || !strings.Contains(string(raw), `"truncated":"true"`) {
		t.Fatalf("line length = %d", len(raw))
	}
	if err := store.Append(context.Background(), ports.JournalEntry{User: "u"}); err == nil {
		t.Fatal("event-less entry accepted")
	}
}
