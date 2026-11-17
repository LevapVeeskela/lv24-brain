package app

import (
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestDumpDocumentMapsRepositoryDumpsToProjects(t *testing.T) {
	byRepo, warnings := projectsByRepo([]domain.Project{
		{ID: "ims", RepoURL: "https://github.com/LevapVeeskela/IMS.git"},
		{ID: "ytk", RepoURL: "git@gitlab.com:lv24/ytk.git"},
		{ID: "local-only"},
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	cases := map[string]string{
		"github:LevapVeeskela/IMS": "ims",
		"gitlab:lv24/ytk":          "ytk",
		"github:someone/else":      "github:someone/else",
		"mail:gmail":               "mail:gmail",
		"":                         "",
	}
	for dumpProject, want := range cases {
		doc := dumpDocument(ports.Dump{Path: "github/x.md", ProjectID: dumpProject, Title: "t"}, byRepo)
		if doc.ProjectID != want || doc.Kind != KindDump || doc.ID != "dump:github/x.md" {
			t.Fatalf("%q -> %+v, want project %q", dumpProject, doc, want)
		}
	}
}

// One repository claimed by two projects is ambiguous: the mapping is empty
// (independent of registry order) and a warning names both candidates, so the
// dump is never attributed to whichever project came last.
func TestProjectsByRepoCollisionIsDeterministic(t *testing.T) {
	projects := []domain.Project{
		{ID: "lv24-ytk-verify-typecheck", RepoURL: "git@gitlab.com:lv24/ytk.git"},
		{ID: "ytk", RepoURL: "https://gitlab.com/lv24/ytk.git"},
	}
	byRepo, warnings := projectsByRepo(projects)
	if len(byRepo) != 0 {
		t.Fatalf("collision mapped to %v, want no project", byRepo)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "lv24-ytk-verify-typecheck, ytk") {
		t.Fatalf("warnings = %v, want one naming both projects in id order", warnings)
	}
	reversed, reversedWarnings := projectsByRepo([]domain.Project{projects[1], projects[0]})
	if len(reversed) != 0 || len(reversedWarnings) != 1 || reversedWarnings[0] != warnings[0] {
		t.Fatalf("reversed input = %v, %v; want %v, %v", reversed, reversedWarnings, byRepo, warnings)
	}
	doc := dumpDocument(ports.Dump{Path: "gitlab/ytk.md", ProjectID: "gitlab:lv24/ytk", Title: "t"}, byRepo)
	if doc.ProjectID != "gitlab:lv24/ytk" {
		t.Fatalf("dump project = %q, want the connector project", doc.ProjectID)
	}
}
