package gitstore

import (
	"context"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestPrepareConnectionPublishUntracksCheckpointsOnly: a connection that
// publishes its items still never publishes checkpoint bytes, including the
// cursors an earlier engine committed. The files stay on disk.
func TestPrepareConnectionPublishUntracksCheckpointsOnly(t *testing.T) {
	requireGit(t)
	repo := initRepo(t, t.TempDir(), false)
	writeFile(t, repo.dir, "connectors/work/items/old.md", "shared item")
	writeFile(t, repo.dir, "connectors/work/checkpoints/inbox-abc.json", `{"stream":"inbox"}`)
	writeFile(t, repo.dir, "connectors/other/checkpoints/inbox-abc.json", `{"stream":"inbox"}`)
	repo.run("add", ".")
	repo.run("commit", "-qm", "seed")
	provider := newTestProvider(t, repo.dir)

	for range 2 {
		if err := provider.PrepareConnectionPublish(context.Background(), "work", domain.PublishShared); err != nil {
			t.Fatal(err)
		}
	}

	tracked := repo.run("ls-files", "connectors")
	if !strings.Contains(tracked, "connectors/work/items/old.md") {
		t.Fatalf("shared items were untracked:\n%s", tracked)
	}
	if strings.Contains(tracked, "connectors/work/checkpoints/") {
		t.Fatalf("checkpoints are still tracked:\n%s", tracked)
	}
	if !strings.Contains(tracked, "connectors/other/checkpoints/") {
		t.Fatalf("another connection was untracked:\n%s", tracked)
	}
	if got := readFile(t, repo.dir, "connectors/work/checkpoints/inbox-abc.json"); got != `{"stream":"inbox"}` {
		t.Fatalf("checkpoint file changed: %q", got)
	}
}
