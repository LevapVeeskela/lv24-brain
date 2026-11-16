package codegraph

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestCodegraphAnalyticsOnTheFixture(t *testing.T) {
	f := newFixture(t)
	f.addNode(ports.GraphSymbol{
		ID: "function:isolated", Kind: "function", Name: "Isolated", QualName: "Isolated",
		File: "internal/api/isolated.go", Language: "go", StartLine: 1, EndLine: 2,
	})
	index := openFixture(t, f)
	ctx := context.Background()

	communities, err := index.Communities(ctx, "demo", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if communities.Total == 0 || len(communities.Communities) == 0 {
		t.Fatalf("communities = %+v", communities)
	}
	status, err := index.Status(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	signatures := map[string]bool{}
	for _, community := range communities.Communities {
		total += community.Size
		if community.Signature == "" || signatures[community.Signature] {
			t.Fatalf("signature = %q", community.Signature)
		}
		signatures[community.Signature] = true
		if community.Cohesion < 0 || community.Cohesion > 1 {
			t.Fatalf("cohesion = %v", community.Cohesion)
		}
		if community.Name == "" || community.Top.ID == "" {
			t.Fatalf("community = %+v", community)
		}
	}
	if total != status.Nodes {
		t.Fatalf("members = %d, nodes = %d", total, status.Nodes)
	}
	selected, err := index.Communities(ctx, "demo", ports.CommunityQuery{ID: 1, MemberLimit: 2})
	if err != nil || len(selected.Communities) != 1 {
		t.Fatalf("selected = %+v err=%v", selected, err)
	}
	if len(selected.Communities[0].Members) == 0 || selected.Communities[0].Members[0].ID == "" {
		t.Fatalf("members = %+v", selected.Communities[0])
	}
	if _, err := index.Communities(ctx, "demo", ports.CommunityQuery{ID: 999}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing community = %v", err)
	}

	god, err := index.GodNodes(ctx, "demo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(god.Nodes) == 0 || god.Total == 0 {
		t.Fatalf("god nodes = %+v", god)
	}
	for _, node := range god.Nodes {
		if node.Kind == "file" || node.Kind == "import" {
			t.Fatalf("a structural node was ranked: %+v", node)
		}
		if node.Degree == 0 || node.Why == "" || node.Community == "" {
			t.Fatalf("god node = %+v", node)
		}
	}
	again, err := index.GodNodes(ctx, "demo", 5)
	if err != nil {
		t.Fatal(err)
	}
	for position := range god.Nodes {
		if god.Nodes[position].ID != again.Nodes[position].ID {
			t.Fatalf("ranking is not deterministic: %v / %v", god.Nodes, again.Nodes)
		}
	}

	// Handler calls ListKeys, which calls hashKey, which calls EventCache.
	path, err := index.ShortestPath(ctx, "demo", "Handler", "EventCache", 5)
	if err != nil || !path.Found {
		t.Fatalf("path = %+v err=%v", path, err)
	}
	if path.Hops != 3 || !path.Directed || len(path.Steps) != 4 {
		t.Fatalf("path = %+v", path)
	}
	if !strings.Contains(path.Steps[3].Name, "EventCache") {
		t.Fatalf("path endpoints = %+v", path.Steps)
	}
	// The reverse direction has no directed path; the answer falls back to
	// undirected edges and says so.
	reverse, err := index.ShortestPath(ctx, "demo", "EventCache", "Handler", 5)
	if err != nil || !reverse.Found {
		t.Fatalf("reverse = %+v err=%v", reverse, err)
	}
	if reverse.Directed {
		t.Fatalf("reverse path claims to be directed: %+v", reverse)
	}
	if !notesContain(reverse.Notes, "reverse") {
		t.Fatalf("reverse notes = %v", reverse.Notes)
	}
	// An isolated node exists and is reachable only from itself.
	missing, err := index.ShortestPath(ctx, "demo", "Handler", "Isolated", 3)
	if err != nil || missing.Found || len(missing.Notes) == 0 {
		t.Fatalf("missing path = %+v err=%v", missing, err)
	}

	stats, err := index.GraphStats(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Symbols != status.Nodes || stats.Files != status.Files || stats.Communities != communities.Total {
		t.Fatalf("stats = %+v status = %+v", stats, status)
	}
	if stats.Orphans == 0 {
		t.Fatalf("the isolated node was not counted as an orphan: %+v", stats)
	}
	if stats.Languages["go"] != status.Files {
		t.Fatalf("languages = %v", stats.Languages)
	}

	if capped, err := index.Communities(ctx, "demo", ports.CommunityQuery{Limit: 1000}); err != nil {
		t.Fatal(err)
	} else if len(capped.Communities) > ports.MaxGraphCommunities {
		t.Fatalf("community cap = %d", len(capped.Communities))
	}
	if capped, err := index.GodNodes(ctx, "demo", 1000); err != nil {
		t.Fatal(err)
	} else if len(capped.Nodes) > ports.MaxGraphGodNodes {
		t.Fatalf("god cap = %d", len(capped.Nodes))
	}
}

func TestCodegraphAnalyticsRefusesAnEmptyIndex(t *testing.T) {
	f := newFixture(t)
	f.nodes = nil
	f.edges = nil
	f.files = map[string][]string{}
	index := openFixture(t, f)
	ctx := context.Background()
	if _, err := index.Communities(ctx, "demo", ports.CommunityQuery{}); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("communities = %v", err)
	}
	if _, err := index.GodNodes(ctx, "demo", 5); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("god nodes = %v", err)
	}
	if _, err := index.GraphStats(ctx, "demo"); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("stats = %v", err)
	}
}

func TestCodegraphPRImpactReadsATempRepository(t *testing.T) {
	index, root, base, head := openCodegraphGitFixture(t)
	ctx := context.Background()

	impact, err := index.PRImpact(ctx, "demo", base, head)
	if err != nil {
		t.Fatal(err)
	}
	if impact.ChangedFiles != 1 || len(impact.Files) != 1 || impact.Files[0].Path != "internal/api/handler.go" {
		t.Fatalf("files = %+v", impact.Files)
	}
	if impact.Files[0].Status != "M" || impact.Files[0].Symbols == 0 {
		t.Fatalf("file = %+v", impact.Files[0])
	}
	if len(impact.ChangedSymbols) == 0 || len(impact.Tests) == 0 {
		t.Fatalf("impact = %+v", impact)
	}
	if contains(impact.Tests, "internal/api/handler_test.go") != true {
		t.Fatalf("tests = %v", impact.Tests)
	}
	if impact.Base != base || impact.Head != head {
		t.Fatalf("refs = %s..%s", impact.Base, impact.Head)
	}

	if err := os.Remove(filepath.Join(root, "internal", "cache", "keys.go")); err != nil {
		t.Fatal(err)
	}
	deleted := codegraphGitCommit(t, root, "delete keys.go")
	deletion, err := index.PRImpact(ctx, "demo", head, deleted)
	if err != nil {
		t.Fatal(err)
	}
	if deletion.Risk != ports.GraphRiskHigh {
		t.Fatalf("deletion risk = %s (%+v)", deletion.Risk, deletion.Notes)
	}
	if len(deletion.Files) != 1 || deletion.Files[0].Status != "D" {
		t.Fatalf("deletion files = %+v", deletion.Files)
	}
	if !notesContain(deletion.Notes, "deleted file") {
		t.Fatalf("deletion notes = %v", deletion.Notes)
	}

	if _, err := index.PRImpact(ctx, "demo", "-x", head); err == nil {
		t.Fatal("an option-shaped ref was accepted")
	}
}

// openCodegraphGitFixture builds the fixture, commits it, edits one file and
// commits again, then opens the index. The index still describes the fixture
// nodes; only the diff comes from git.
func openCodegraphGitFixture(t *testing.T) (*Index, string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := newFixture(t)
	root, dbPath := f.build()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".codegraph/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codegraphGit(t, root, "init", "-q", "-b", "main")
	base := codegraphGitCommit(t, root, "base")

	path := filepath.Join(root, "internal", "api", "handler.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("\n// Handler changed between the two refs.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	head := codegraphGitCommit(t, root, "change handler")

	index, err := Open(context.Background(), Options{DBPath: dbPath, Root: root, Project: "demo"})
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return index, root, base, head
}

func codegraphGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func codegraphGitCommit(t *testing.T, root, message string) string {
	t.Helper()
	codegraphGit(t, root, "add", "-A")
	codegraphGit(t, root, "-c", "user.email=fixture@example.com", "-c", "user.name=Fixture",
		"commit", "-q", "-m", message)
	command := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func notesContain(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}
