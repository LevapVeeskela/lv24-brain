package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// failingProblemStore makes the problem scan observable. The session brief must
// never call it: a broken scanner cannot break a session start.
type failingProblemStore struct {
	*fsstore.Store
}

func (s failingProblemStore) Problems(context.Context, string) ([]ports.RecordProblem, error) {
	return nil, errors.New("problem scan should not run")
}

func TestIdeaBriefSkipsProblemScan(t *testing.T) {
	ctx := context.Background()
	_, store := newIdeaApp(t)
	seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: failingProblemStore{Store: store}})
	if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: "p", Title: "Brief me"}); err != nil {
		t.Fatalf("AddIdea: %v", err)
	}

	if _, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: "p"}); err == nil {
		t.Fatalf("ListIdeas did not call the problem reporter")
	}
	if _, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: "p", SkipProblems: true}); err != nil {
		t.Fatalf("ListIdeas(SkipProblems) = %v", err)
	}
	brief, err := a.IdeaBrief(ctx, app.IdeaBriefOptions{ProjectID: "p", Scope: app.IdeaBriefScopeCurrent})
	if err != nil {
		t.Fatalf("IdeaBrief ran the problem scan: %v", err)
	}
	if brief.Open != 1 || !strings.Contains(brief.Text, "Brief me") {
		t.Fatalf("brief = %+v", brief)
	}
}
