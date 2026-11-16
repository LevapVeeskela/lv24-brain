package codeindex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestAnalyticsOnTheFixtureModule exercises the native analytics over the
// fixture's real go/types graph: communities, god nodes, path and stats.
func TestAnalyticsOnTheFixtureModule(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()

	communities, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if communities.Total == 0 || len(communities.Communities) == 0 {
		t.Fatalf("communities = %+v", communities)
	}
	status, err := fixture.index.Status(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	seen := map[string]bool{}
	for _, community := range communities.Communities {
		total += community.Size
		if community.Signature == "" || seen[community.Signature] {
			t.Fatalf("community signature = %q", community.Signature)
		}
		seen[community.Signature] = true
		if community.Cohesion < 0 || community.Cohesion > 1 {
			t.Fatalf("cohesion = %v", community.Cohesion)
		}
		if community.Top.ID == "" || community.Name == "" {
			t.Fatalf("community has no top: %+v", community)
		}
	}
	if total != status.Nodes {
		t.Fatalf("members = %d, symbols = %d", total, status.Nodes)
	}
	selected, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{
		ID: communities.Communities[0].ID, MemberLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Communities) != 1 || len(selected.Communities[0].Members) == 0 {
		t.Fatalf("selected = %+v", selected)
	}
	if selected.Communities[0].Signature != communities.Communities[0].Signature {
		t.Fatalf("selected signature moved: %+v", selected.Communities[0])
	}
	if selected.Communities[0].MembersTruncated != (selected.Communities[0].Size > 2) {
		t.Fatalf("member truncation = %+v", selected.Communities[0])
	}
	if _, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{ID: 9999}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing community = %v", err)
	}

	god, err := fixture.index.GodNodes(ctx, "fixture", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(god.Nodes) == 0 || god.Total == 0 {
		t.Fatalf("god nodes = %+v", god)
	}
	for _, node := range god.Nodes {
		if node.Kind == "package" {
			t.Fatalf("a package node was ranked: %+v", node)
		}
		if node.Degree == 0 || node.Why == "" || node.Community == "" {
			t.Fatalf("god node = %+v", node)
		}
	}
	again, err := fixture.index.GodNodes(ctx, "fixture", 5)
	if err != nil {
		t.Fatal(err)
	}
	for index := range god.Nodes {
		if god.Nodes[index].ID != again.Nodes[index].ID {
			t.Fatalf("god ranking is not deterministic: %v / %v", god.Nodes, again.Nodes)
		}
	}

	path, err := fixture.index.ShortestPath(ctx, "fixture", "New", "NewMemoryStore", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !path.Found || path.Hops == 0 || len(path.Steps) != path.Hops+1 {
		t.Fatalf("path = %+v", path)
	}
	if path.Steps[0].Name != "New" || !strings.Contains(path.Steps[len(path.Steps)-1].Name, "NewMemoryStore") {
		t.Fatalf("path endpoints = %+v", path.Steps)
	}
	if path.Confidence <= 0 || path.Confidence > 1 {
		t.Fatalf("path confidence = %v", path.Confidence)
	}
	if missing, err := fixture.index.ShortestPath(ctx, "fixture", "BodyOnly", "cache.JSONCodec.Encode", 1); err != nil || missing.Found || len(missing.Notes) == 0 {
		t.Fatalf("missing path = %+v err=%v", missing, err)
	}
	if clamped, err := fixture.index.ShortestPath(ctx, "fixture", "New", "NewMemoryStore", 100); err != nil {
		t.Fatal(err)
	} else if len(clamped.Notes) == 0 || len(clamped.Steps) > ports.MaxGraphPathNodes {
		t.Fatalf("clamped path = %+v", clamped)
	}

	stats, err := fixture.index.GraphStats(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Symbols != status.Nodes || stats.Files != status.Files || stats.Communities != communities.Total {
		t.Fatalf("stats = %+v, status = %+v", stats, status)
	}
	if stats.Languages["go"] != status.Files {
		t.Fatalf("languages = %v", stats.Languages)
	}
	if stats.Orphans < 0 || stats.Orphans > stats.Symbols {
		t.Fatalf("orphans = %d", stats.Orphans)
	}

	capped, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Communities) > ports.MaxGraphCommunities {
		t.Fatalf("communities cap = %d", len(capped.Communities))
	}
	if capped, err := fixture.index.GodNodes(ctx, "fixture", 1000); err != nil {
		t.Fatal(err)
	} else if len(capped.Nodes) > ports.MaxGraphGodNodes {
		t.Fatalf("god cap = %d", len(capped.Nodes))
	}
}

func TestKnownCommunitiesGodNodesAndPath(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()

	// Two directed five-cliques joined by one bridge:
	// A0..A4 (all i<j), B5..B9 (all i<j), plus A2 -> B5.
	names := []string{"A0", "A1", "A2", "A3", "A4", "B5", "B6", "B7", "B8", "B9"}
	ids := make([]string, len(names))
	for index, name := range names {
		ids[index] = "sym_analytics_" + strings.ToLower(name)
		if _, err := fixture.index.db.ExecContext(ctx,
			`INSERT INTO symbols(id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported)
			 VALUES(?, 'synthetic/cluster.go', 'synthetic', 'func', ?, ?, ?, ?, '', '', 0)`,
			ids[index], name, name, index+1, index+1); err != nil {
			t.Fatal(err)
		}
	}
	edges := make([][2]string, 0, 21)
	for left := 0; left < 5; left++ {
		for right := left + 1; right < 5; right++ {
			edges = append(edges, [2]string{ids[left], ids[right]})
		}
	}
	for left := 5; left < 10; left++ {
		for right := left + 1; right < 10; right++ {
			edges = append(edges, [2]string{ids[left], ids[right]})
		}
	}
	edges = append(edges, [2]string{ids[2], ids[5]})
	for _, edge := range edges {
		if _, err := fixture.index.db.ExecContext(ctx,
			`INSERT INTO edges(source, target, kind, line, confidence, provenance)
			 VALUES(?, ?, 'calls', 1, 1.0, 'types')`, edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := fixture.index.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	count, err := rebuildCommunities(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Fatalf("communities = %d", count)
	}

	// The two five-cliques are dense groups: all five members share a
	// community, the two groups differ, and each clique has cohesion 1.
	communityOf := map[string]int{}
	cohesion := map[int]float64{}
	rows, err := fixture.index.db.QueryContext(ctx,
		`SELECT cm.symbol_id, cm.community_id, c.cohesion FROM community_members cm
		 JOIN communities c ON c.id = cm.community_id
		 WHERE cm.symbol_id LIKE 'sym_analytics_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var community int
		var value float64
		if err := rows.Scan(&id, &community, &value); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		communityOf[id] = community
		cohesion[community] = value
	}
	_ = rows.Close()
	for index := 1; index < 5; index++ {
		if communityOf[ids[index]] != communityOf[ids[0]] {
			t.Fatalf("clique A split: %v", communityOf)
		}
		if communityOf[ids[5+index]] != communityOf[ids[5]] {
			t.Fatalf("clique B split: %v", communityOf)
		}
	}
	if communityOf[ids[0]] == communityOf[ids[5]] {
		t.Fatalf("the bridge merged both cliques: %v", communityOf)
	}
	for _, id := range []string{ids[0], ids[5]} {
		if value := cohesion[communityOf[id]]; value != 1 {
			t.Fatalf("cohesion of clique %s = %v", id, value)
		}
	}

	god, err := fixture.index.GodNodes(ctx, "fixture", ports.MaxGraphGodNodes)
	if err != nil {
		t.Fatal(err)
	}
	ranked := map[string]int{}
	for _, node := range god.Nodes {
		ranked[node.ID] = node.Degree
	}
	for _, id := range []string{ids[2], ids[5]} {
		if ranked[id] != 5 {
			t.Fatalf("god degree of %s = %d, want 5 (%+v)", id, ranked[id], god.Nodes)
		}
	}
	if ranked[ids[0]] != 4 {
		t.Fatalf("god degree of %s = %d, want 4", ids[0], ranked[ids[0]])
	}

	path, err := fixture.index.ShortestPath(ctx, "fixture", "A0", "B7", 6)
	if err != nil || !path.Found || !path.Directed || path.Hops != 3 {
		t.Fatalf("path = %+v err=%v", path, err)
	}
	want := []string{"A0", "A2", "B5", "B7"}
	for index, name := range want {
		if path.Steps[index].Name != name {
			t.Fatalf("path step %d = %s, want %s (%+v)", index, path.Steps[index].Name, name, path.Steps)
		}
	}

	// Recomputing the partition preserves ids for unchanged signatures.
	tx, err = fixture.index.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildCommunities(ctx, tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	again := map[string]int{}
	rows, err = fixture.index.db.QueryContext(ctx,
		`SELECT symbol_id, community_id FROM community_members WHERE symbol_id LIKE 'sym_analytics_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var community int
		if err := rows.Scan(&id, &community); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		again[id] = community
	}
	_ = rows.Close()
	for id, community := range communityOf {
		if again[id] != community {
			t.Fatalf("community id of %s moved: %d -> %d", id, community, again[id])
		}
	}
}

func TestCommunityIDsSurviveAnUnchangedSync(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	before, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	bySignature := map[string]int{}
	for _, community := range before.Communities {
		bySignature[community.Signature] = community.ID
	}

	// A comment-only edit changes the file hash and nothing in the graph.
	path := filepath.Join(fixture.root, "cache", "cache.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("\n// A comment-only edit keeps every symbol and edge valid.\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.mustSync(t)
	after, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, community := range after.Communities {
		if previous, ok := bySignature[community.Signature]; ok && previous != community.ID {
			t.Fatalf("community %q moved from id %d to %d", community.Signature, previous, community.ID)
		}
	}
	if after.Total != before.Total {
		t.Fatalf("communities changed on a comment-only sync: %d -> %d", before.Total, after.Total)
	}
}

func TestAnalyticsRefusesANonGoCheckout(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "docs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# not a Go module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath, err := DefaultDBPath(filepath.Join(base, "brain"), "docs", root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := Open(context.Background(), Options{Root: root, DBPath: dbPath, Project: "docs", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err := index.Communities(context.Background(), "docs", ports.CommunityQuery{}); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("non-Go communities = %v", err)
	}
	if _, err := index.GraphStats(context.Background(), "docs"); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("non-Go stats = %v", err)
	}
}

func TestAnalyticsOnAnEmptyStoreIsUnsupported(t *testing.T) {
	fixture := newFixture(t)
	ctx := context.Background()
	if _, err := fixture.index.Communities(ctx, "fixture", ports.CommunityQuery{}); !errors.Is(err, ErrIndexEmpty) {
		t.Fatalf("communities on an empty store = %v", err)
	}
	if _, err := fixture.index.GraphStats(ctx, "fixture"); !errors.Is(err, ErrIndexEmpty) {
		t.Fatalf("stats on an empty store = %v", err)
	}
}

// TestPRImpactReadsATempRepository builds a git history, indexes its head and
// checks the diff mapping, the risk tier and the test discovery.
func TestPRImpactReadsATempRepository(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	fixture.mustIndex(t)

	impact, err := fixture.index.PRImpact(ctx, "fixture", fixture.base, fixture.head)
	if err != nil {
		t.Fatal(err)
	}
	if impact.ChangedFiles != 1 || len(impact.Files) != 1 || impact.Files[0].Path != "service/service.go" {
		t.Fatalf("files = %+v", impact.Files)
	}
	if impact.Files[0].Status != "M" || impact.Files[0].Symbols == 0 {
		t.Fatalf("file = %+v", impact.Files[0])
	}
	if len(impact.ChangedSymbols) == 0 {
		t.Fatalf("changed symbols = %+v", impact)
	}
	if len(impact.Impact) == 0 || impact.TotalImpact == 0 {
		t.Fatalf("impact = %+v", impact)
	}
	if !contains(impact.Tests, "service/service_test.go") {
		t.Fatalf("tests = %v", impact.Tests)
	}
	if impact.Risk != ports.GraphRiskLow {
		t.Fatalf("risk = %s (%+v)", impact.Risk, impact.Notes)
	}
	if impact.Base != fixture.base || impact.Head != fixture.head {
		t.Fatalf("refs = %s..%s", impact.Base, impact.Head)
	}

	// A deleted file is always high risk, and the answer says why.
	if err := os.Remove(filepath.Join(fixture.root, "cache", "generic.go")); err != nil {
		t.Fatal(err)
	}
	deleted := gitCommit(t, fixture.root, "delete cache/generic.go")
	deletion, err := fixture.index.PRImpact(ctx, "fixture", fixture.head, deleted)
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

	if _, err := fixture.index.PRImpact(ctx, "fixture", "-x", fixture.head); err == nil {
		t.Fatal("an option-shaped ref was accepted")
	}
	if _, err := fixture.index.PRImpact(ctx, "fixture", "", fixture.head); err == nil {
		t.Fatal("an empty base was accepted")
	}
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

// gitFixture is a checkout with two commits and an open native index.
type gitFixture struct {
	*fixtureIndex
	base string
	head string
}

// newGitFixture writes the fixture module, commits it, edits one file and
// commits again, then opens the index the way a user would after the change.
func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	baseDir := t.TempDir()
	root := filepath.Join(baseDir, "repo")
	data := filepath.Join(baseDir, "brain")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root)
	git(t, root, "init", "-q", "-b", "main")
	base := gitCommit(t, root, "base")

	path := filepath.Join(root, "service", "service.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n// Fetched documents that the fixture changed between the two refs.\n")...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	head := gitCommit(t, root, "change service")

	now := time.Unix(1_700_000_000, 0).UTC()
	dbPath, err := DefaultDBPath(data, "fixture", root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := Open(context.Background(), Options{
		Root: root, DBPath: dbPath, Project: "fixture", Create: true,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return &gitFixture{
		fixtureIndex: &fixtureIndex{index: index, dbPath: dbPath, root: root, data: data, now: now},
		base:         base,
		head:         head,
	}
}

func git(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func gitCommit(t *testing.T, root, message string) string {
	t.Helper()
	git(t, root, "add", "-A")
	git(t, root, "-c", "user.email=fixture@example.com", "-c", "user.name=Fixture",
		"commit", "-q", "-m", message)
	command := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
