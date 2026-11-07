package app

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestDumpDocumentMapsRepositoryDumpsToProjects(t *testing.T) {
	byRepo := projectsByRepo([]domain.Project{
		{ID: "ims", RepoURL: "https://github.com/LevapVeeskela/IMS.git"},
		{ID: "ytk", RepoURL: "git@gitlab.com:lv24/ytk.git"},
		{ID: "local-only"},
	})
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
