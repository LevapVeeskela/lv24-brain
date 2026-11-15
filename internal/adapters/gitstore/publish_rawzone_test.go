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

// TestRawZoneTrackedListsOnlyItemsAndCheckpoints: the grandfather check must
// see published raw files and ignore the rest of a connection directory.
func TestRawZoneTrackedListsOnlyItemsAndCheckpoints(t *testing.T) {
	requireGit(t)
	repo := initRepo(t, t.TempDir(), false)
	writeFile(t, repo.dir, "connectors/work/items/old.md", "item")
	writeFile(t, repo.dir, "connectors/work/checkpoints/inbox-abc.json", `{"stream":"inbox"}`)
	writeFile(t, repo.dir, "connectors/work/settings.json", "{}")
	writeFile(t, repo.dir, "connectors/other/items/old.md", "other")
	repo.run("add", ".")
	repo.run("commit", "-qm", "seed")
	provider := newTestProvider(t, repo.dir)
	ctx := context.Background()

	paths, err := provider.TrackedRawZonePaths(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("tracked raw paths = %v", paths)
	}
	for _, path := range paths {
		if strings.Contains(path, "settings.json") {
			t.Fatalf("non-raw file reported: %v", paths)
		}
	}
	for connectionID, want := range map[string]bool{"work": true, "other": true, "fresh": false} {
		tracked, err := provider.RawZoneTracked(ctx, connectionID)
		if err != nil || tracked != want {
			t.Fatalf("RawZoneTracked(%q) = %t, %v; want %t", connectionID, tracked, err, want)
		}
	}
}
