package gitstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestPrepareConnectionPublishUntracksBeforePrivateWrites(t *testing.T) {
	requireGit(t)
	repo := initRepo(t, t.TempDir(), false)
	writeFile(t, repo.dir, "connectors/work/items/old.md", "shared")
	writeFile(t, repo.dir, "connectors/other/items/old.md", "other")
	repo.run("add", ".")
	repo.run("commit", "-qm", "seed")
	provider := newTestProvider(t, repo.dir)
	for range 2 {
		if err := provider.PrepareConnectionPublish(context.Background(), "work", domain.PublishLocalOnly); err != nil {
			t.Fatal(err)
		}
	}
	tracked := repo.run("ls-files", "connectors")
	if strings.Contains(tracked, "connectors/work/") || !strings.Contains(tracked, "connectors/other/") {
		t.Fatal(tracked)
	}
	if got := readFile(t, repo.dir, "connectors/work/items/old.md"); got != "shared" {
		t.Fatal(got)
	}
	if err := provider.PrepareConnectionPublish(context.Background(), "../other", domain.PublishLocalOnly); !errors.Is(err, domain.ErrValidation) {
		t.Fatal(err)
	}
}
