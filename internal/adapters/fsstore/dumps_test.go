package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDump(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListDumpsReadsConnectorAndLegacyFiles(t *testing.T) {
	store := newTestStore(t)
	writeDump(t, store.root, "github/acme/widgets.md",
		"---\nkind: \"connector_dump\"\nproject_id: \"github:acme/widgets\"\nupdated_at: \"2026-11-06T20:16:30Z\"\n---\n\n# acme/widgets\n\n- Issue: flaky build\n")
	writeDump(t, store.root, "mail/gmail/2026-10-18.md",
		"---\nkind: mail\naccount: gmail\nproject_id: null\ndate: 2026-10-18\n---\n\n# Mail gmail 2026-10-18\n\n## 00:50 - Security alert\n")
	writeDump(t, store.root, "mail/corp/inbox/2026-11-07-uid3-4.md", "no frontmatter at all\n")
	writeDump(t, store.root, "mail/corp/inbox/empty.md", "")
	writeDump(t, store.root, "mail/corp/notes.txt", "ignored")

	dumps, err := store.ListDumps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]int{}
	for i, dump := range dumps {
		byPath[dump.Path] = i
	}
	if len(dumps) != 3 {
		t.Fatalf("dumps = %+v", dumps)
	}
	github := dumps[byPath["github/acme/widgets.md"]]
	if github.ProjectID != "github:acme/widgets" || github.Title != "acme/widgets" ||
		!github.UpdatedAt.Equal(time.Date(2026, 11, 6, 20, 16, 30, 0, time.UTC)) || !strings.Contains(github.Body, "flaky build") {
		t.Fatalf("github dump = %+v", github)
	}
	legacy := dumps[byPath["mail/gmail/2026-10-18.md"]]
	if legacy.ProjectID != "mail:gmail" || legacy.Title != "Mail gmail 2026-10-18" || !legacy.UpdatedAt.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("legacy mail dump = %+v", legacy)
	}
	plain := dumps[byPath["mail/corp/inbox/2026-11-07-uid3-4.md"]]
	if plain.Body != "no frontmatter at all\n" || plain.Title != "mail/corp/inbox/2026-11-07-uid3-4.md" {
		t.Fatalf("plain dump = %+v", plain)
	}
}
